package fusefs

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/hiver-sh/hiver/internal/remotefs"
)

// OpType names the kind of mutation an [OplogEntry] encodes.
type OpType string

const (
	OpPut    OpType = "put"
	OpDelete OpType = "delete"
	OpMove   OpType = "move"
)

// OplogEntry is one filesystem mutation pending replay to a [remotefs.Store].
//
// Path / NewPath are the agent-visible paths (rooted at fs.mount). For
// OpPut the content is read at flush time from BufferPath, which is
// the corresponding location in the local buffer — so repeated writes
// between the enqueue and the flush coalesce naturally (we always send
// the latest bytes, never a stale snapshot).
type OplogEntry struct {
	Type       OpType
	Path       string
	NewPath    string // OpMove only
	BufferPath string // OpPut only — local file to read content from
	At         time.Time
}

func (o OplogEntry) String() string {
	switch o.Type {
	case OpMove:
		return fmt.Sprintf("%s %s → %s", o.Type, o.Path, o.NewPath)
	default:
		return fmt.Sprintf("%s %s", o.Type, o.Path)
	}
}

// Oplog queues filesystem mutations and replays them asynchronously to
// a [remotefs.Store]. fusefs handlers call [Oplog.Enqueue] after a
// local mutation succeeds; an uploader goroutine started by [Oplog.Run]
// drains the queue.
//
// Failures land in a dead-letter list (inspectable via [Oplog.Dead]); a
// follow-up will add disk-spilling + retry-with-backoff.
type Oplog struct {
	store remotefs.Store
	queue chan OplogEntry

	// keepBuffer disables the post-Put eviction of the local buffer file.
	// Set (before Run starts) for async mounts, where the read handlers
	// serve the local buffer exclusively and never fall back to the
	// remote — evicting would make every flushed file vanish from the
	// agent's view. See [Mount].
	keepBuffer bool

	// coalesceWindow debounces OpPut replay per path. A remote object store
	// has no append: every write-through is a full-object PUT. An agent that
	// builds a file incrementally — open, append a chunk, close, repeat —
	// closes the handle N times, and each close is one whole-object upload,
	// so an N-append file costs N PUTs and re-uploads O(N²) bytes. The window
	// holds a freshly enqueued Put back this long, and a follow-up Put to the
	// same path within the window collapses into it (the buffer file already
	// holds the latest bytes, read at flush time), so the burst becomes a
	// single upload. coalesceMax caps the total hold so a file under sustained
	// appends (a log) still syncs periodically instead of never. A zero window
	// disables coalescing — Puts enqueue immediately (the pre-coalesce path).
	// Delete/Move force a path's pending Put out ahead of them, preserving the
	// store's Put-before-mutation ordering.
	coalesceWindow time.Duration
	coalesceMax    time.Duration

	pendMu  sync.Mutex
	pending map[string]*pendingPut // agent-visible path → debounced OpPut

	mu    sync.Mutex
	dead  []OplogEntry
	dirty map[string]int // agent-visible path → outstanding Op count
}

// pendingPut is an OpPut parked on the coalesce window. bufferPath tracks the
// latest write's buffer location — content is read at flush time, so a
// coalesced group always uploads the newest bytes. firstAt anchors the
// coalesceMax cap; timer fires the deferred enqueue.
type pendingPut struct {
	bufferPath string
	firstAt    time.Time
	timer      *time.Timer
}

// DefaultCoalesceWindow / DefaultCoalesceMax tune OpPut debouncing (see
// Oplog.coalesceWindow). The window is wide enough to absorb a typical
// incremental-append burst (closes hundreds of ms apart) into one upload,
// while the cap guarantees a continuously appended file still syncs at least
// this often.
const (
	DefaultCoalesceWindow = 750 * time.Millisecond
	DefaultCoalesceMax    = 3 * time.Second
)

// NewOplog returns an Oplog that will replay to store. depth is the
// channel buffer; an Enqueue on a full queue blocks the FUSE handler.
// OpPut coalescing is on by default (see [Oplog.SetCoalesce]).
func NewOplog(store remotefs.Store, depth int) *Oplog {
	return &Oplog{
		store:          store,
		queue:          make(chan OplogEntry, depth),
		dirty:          make(map[string]int),
		pending:        make(map[string]*pendingPut),
		coalesceWindow: DefaultCoalesceWindow,
		coalesceMax:    DefaultCoalesceMax,
	}
}

// SetCoalesce overrides the OpPut debounce window and cap. A window <= 0
// disables coalescing, so every Put enqueues immediately. Must be called
// before Run (and before any Enqueue), while the Oplog is idle.
func (o *Oplog) SetCoalesce(window, max time.Duration) {
	o.coalesceWindow = window
	o.coalesceMax = max
}

// Enqueue submits an entry to the uploader. Blocks if the queue is full.
// The entry's primary path (and NewPath, for OpMove) is marked dirty so
// fusefs read paths know to serve from the local buffer instead of
// re-fetching from the remote — pending writes haven't been uploaded
// yet, so the remote's view is staler than the buffer's.
func (o *Oplog) Enqueue(e OplogEntry) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if o.coalesceWindow > 0 && e.Type == OpPut {
		o.debouncePut(e)
		return
	}
	// A Delete or Move must not overtake a still-debounced Put for the same
	// path, or the store would apply them out of order (delete-then-put
	// resurrects the file; move-then-put drops the moved content). Force any
	// pending Put(s) into the queue first, so the store still sees
	// Put-before-mutation.
	switch e.Type {
	case OpDelete:
		o.forcePending(e.Path)
	case OpMove:
		o.forcePending(e.Path)
		if e.NewPath != "" {
			o.forcePending(e.NewPath)
		}
	}
	o.markDirty(e)
	o.queue <- e
}

// debouncePut coalesces an OpPut. If a Put for the same path is already
// parked, it points the group at the latest write's buffer and extends the
// window (bounded by coalesceMax from the group's start). Otherwise it marks
// the path dirty once — matched by the single markClean when the group's Put
// finally flushes, so IsDirty stays balanced and reads keep serving the local
// buffer until the upload lands — and starts the timer.
func (o *Oplog) debouncePut(e OplogEntry) {
	o.pendMu.Lock()
	defer o.pendMu.Unlock()
	if p, ok := o.pending[e.Path]; ok {
		p.bufferPath = e.BufferPath
		p.timer.Reset(o.debounceDelay(p.firstAt))
		return
	}
	path := e.Path
	p := &pendingPut{bufferPath: e.BufferPath, firstAt: e.At}
	p.timer = time.AfterFunc(o.debounceDelay(p.firstAt), func() { o.firePending(path) })
	o.pending[path] = p
	o.markDirty(e)
}

// debounceDelay is how long a group that started at firstAt still waits: the
// full window, unless that overruns the coalesceMax cap, in which case it
// fires at the cap (never negative).
func (o *Oplog) debounceDelay(firstAt time.Time) time.Duration {
	d := o.coalesceWindow
	if rem := time.Until(firstAt.Add(o.coalesceMax)); rem < d {
		d = rem
	}
	if d < 0 {
		d = 0
	}
	return d
}

// firePending is the debounce-timer callback: it removes the pending Put for
// path and hands it to the uploader. A no-op if the entry was already taken
// (by forcePending or the shutdown drain). The path stays dirty — it was
// marked when the group began; the flush's markClean balances it. The queue
// send is done outside pendMu so a full queue can't stall a concurrent
// debouncePut/forcePending.
func (o *Oplog) firePending(path string) {
	o.pendMu.Lock()
	p, ok := o.pending[path]
	if ok {
		delete(o.pending, path)
	}
	o.pendMu.Unlock()
	if !ok {
		return
	}
	o.queue <- OplogEntry{Type: OpPut, Path: path, BufferPath: p.bufferPath, At: time.Now()}
}

// forcePending flushes any debounced Put for path into the queue immediately,
// used before a Delete/Move so the store applies operations in order. No-op if
// nothing is parked for path.
func (o *Oplog) forcePending(path string) {
	o.pendMu.Lock()
	p, ok := o.pending[path]
	if ok {
		delete(o.pending, path)
		p.timer.Stop()
	}
	o.pendMu.Unlock()
	if !ok {
		return
	}
	o.queue <- OplogEntry{Type: OpPut, Path: path, BufferPath: p.bufferPath, At: time.Now()}
}

// RenameParkedPut redirects a still-parked OpPut from oldPath to newPath, whose
// content now lives at newBufferPath — the local buffer was just renamed by a
// FUSE Rename. It reports whether a parked Put for oldPath existed and was
// redirected; when it did, the caller must NOT also enqueue an OpMove.
//
// This is the temp-file-then-rename atomic write (editors, snapshot.Capture):
// the source's whole-object Put is still debounced, so nothing was uploaded to
// the remote under the temp name — an OpMove would reference a remote object
// that doesn't exist, and the parked Put's BufferPath (the temp) no longer
// exists locally either (Rename moved it to newBufferPath). Redirecting the
// write to land directly as newPath is both correct and optimal: one whole-
// object PUT of the final name, no phantom temp upload + move.
//
// The redirected write is enqueued immediately rather than re-parked: a rename
// is a deliberate commit point, not the incremental-append burst coalescing
// exists to absorb, so there is nothing to gain by holding it longer. Returns
// false when no Put is parked for oldPath (its Put already reached the queue or
// the remote), leaving the caller to fall back to an OpMove.
func (o *Oplog) RenameParkedPut(oldPath, newPath, newBufferPath string) bool {
	o.pendMu.Lock()
	p, ok := o.pending[oldPath]
	if !ok {
		o.pendMu.Unlock()
		return false
	}
	delete(o.pending, oldPath)
	p.timer.Stop()
	// A parked Put for the destination (renaming over a not-yet-uploaded file)
	// is superseded by this content — drop it so it doesn't double-upload, and
	// reuse its dirty count for the single Put we enqueue below.
	newHadParked := false
	if q, ok2 := o.pending[newPath]; ok2 {
		q.timer.Stop()
		delete(o.pending, newPath)
		newHadParked = true
	}
	o.pendMu.Unlock()

	// Retire the temp's dirty count (its Put is dropped, never flushed) and give
	// newPath exactly one outstanding count, matched by the markClean when the
	// enqueued Put flushes — so IsDirty stays balanced and reads keep serving the
	// local buffer until the upload lands.
	o.mu.Lock()
	if o.dirty[oldPath] > 0 {
		o.dirty[oldPath]--
		if o.dirty[oldPath] == 0 {
			delete(o.dirty, oldPath)
		}
	}
	if !newHadParked {
		o.dirty[newPath]++
	}
	o.mu.Unlock()

	o.queue <- OplogEntry{Type: OpPut, Path: newPath, BufferPath: newBufferPath, At: time.Now()}
	return true
}

// takePending removes and returns every debounced Put as a ready-to-flush
// entry, stopping its timer. Used by the shutdown drain, which flushes them
// directly rather than via the queue (Run has stopped consuming, so a full
// queue would deadlock). A timer that already fired may have queued its entry;
// deleting from the map makes that firePending a no-op, and a duplicate
// whole-object Put is idempotent, so no write is lost.
func (o *Oplog) takePending() []OplogEntry {
	o.pendMu.Lock()
	defer o.pendMu.Unlock()
	out := make([]OplogEntry, 0, len(o.pending))
	for path, p := range o.pending {
		p.timer.Stop()
		out = append(out, OplogEntry{Type: OpPut, Path: path, BufferPath: p.bufferPath, At: time.Now()})
	}
	o.pending = make(map[string]*pendingPut)
	return out
}

// IsDirty reports whether path has at least one Op pending or in flight
// (Enqueue'd, not yet flushed). fusefs read paths consult this to choose
// "serve local buffer" vs "fetch from remote".
func (o *Oplog) IsDirty(path string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.dirty[path] > 0
}

func (o *Oplog) markDirty(e OplogEntry) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dirty[e.Path]++
	if e.Type == OpMove && e.NewPath != "" {
		o.dirty[e.NewPath]++
	}
}

func (o *Oplog) markClean(e OplogEntry) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.dirty[e.Path] > 0 {
		o.dirty[e.Path]--
		if o.dirty[e.Path] == 0 {
			delete(o.dirty, e.Path)
		}
	}
	if e.Type == OpMove && e.NewPath != "" && o.dirty[e.NewPath] > 0 {
		o.dirty[e.NewPath]--
		if o.dirty[e.NewPath] == 0 {
			delete(o.dirty, e.NewPath)
		}
	}
}

// Run drains the oplog until ctx is cancelled, then flushes any
// remaining queued entries with a fresh, bounded context before
// returning. The shutdown drain matters for remote backends (gdrive,
// future s3/gcs/onedrive): we don't want to lose writes the agent
// thinks succeeded just because sbxfuse is being torn down — the
// FUSE Write returned ok the moment the local buffer accepted it.
func (o *Oplog) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			o.drainOnShutdown()
			return
		case e := <-o.queue:
			o.flush(ctx, e)
		}
	}
}

// shutdownDrainTimeout caps how long Run will wait for the queue to
// empty after ctx cancellation. Bounded so a hung remote (network
// down, API rate limit) doesn't keep sbxfuse alive past sandboxd's
// kill timeout.
const shutdownDrainTimeout = 5 * time.Second

func (o *Oplog) drainOnShutdown() {
	drainCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()
	// Coalesced Puts still parked on their debounce timers haven't reached the
	// queue. Stop the timers and flush them directly — routing through the
	// queue would deadlock, since Run (the only consumer) has already returned
	// into this drain.
	pending := o.takePending()
	for _, e := range pending {
		o.flush(drainCtx, e)
	}
	drained := len(pending)
	for {
		select {
		case e := <-o.queue:
			o.flush(drainCtx, e)
			drained++
		case <-drainCtx.Done():
			log.Printf("oplog: shutdown drain timed out after %v with %d remaining", shutdownDrainTimeout, len(o.queue))
			return
		default:
			if drained > 0 {
				log.Printf("oplog: flushed %d pending entries on shutdown", drained)
			}
			return
		}
	}
}

// flush replays one entry against the store. On error the entry is
// appended to the dead-letter list — callers inspect via [Oplog.Dead].
//
// On success the entry's path is marked clean and any local buffer file
// for OpPut is evicted: the remote now holds the canonical content, so
// keeping a copy in the write buffer would just make the buffer dir
// double as a stale read cache (the very thing we explicitly removed
// when we deleted Bootstrap). Subsequent reads consult the remote
// directly via [Config.Remote]. Async mounts are the exception: their
// read handlers never consult the remote, so keepBuffer leaves the
// local copy in place — there, the buffer IS the authoritative view.
func (o *Oplog) flush(ctx context.Context, e OplogEntry) {
	// Detach the remote operation from ctx's cancellation. ctx is the uploader's
	// lifecycle context: it is cancelled at shutdown, which is exactly when an
	// upload may be in flight (e.g. a snapshot tarball written through a FUSE
	// drive moments before teardown). Cancelling mid-request would abort that
	// upload and dead-letter it, silently losing a write the FUSE Write already
	// acked. The request stays bounded by the remote HTTP client's own per-call
	// timeout (and, on shutdown, by sandboxd's SIGKILL grace), so detaching can't
	// hang teardown indefinitely.
	opCtx := context.WithoutCancel(ctx)
	var err error
	switch e.Type {
	case OpPut:
		var f *os.File
		f, err = os.Open(e.BufferPath)
		if err != nil {
			if os.IsNotExist(err) {
				// The buffer file is gone. Two ways this happens, both
				// are a clean skip rather than a failure:
				//   1. A previous flush for the same path already
				//      uploaded + evicted the buffer (e.g. a Create
				//      enqueue followed by a Write enqueue, both for
				//      the same path; never with keepBuffer set).
				//   2. The agent removed the file between Write and
				//      our flush.
				// Either way the remote either already has the latest
				// content or will receive it from a later Op. Don't
				// dead-letter — that would surface a phantom error.
				log.Printf("oplog: skip put %s: buffer evicted", e.Path)
				o.markClean(e)
				return
			}
		} else {
			err = o.store.Put(opCtx, e.Path, f)
			f.Close()
		}
	case OpDelete:
		err = o.store.Delete(opCtx, e.Path)
	case OpMove:
		err = o.store.Move(opCtx, e.Path, e.NewPath)
	default:
		err = fmt.Errorf("unknown op %q", e.Type)
	}
	if err != nil {
		log.Printf("oplog: %s: %v", e, err)
		o.mu.Lock()
		o.dead = append(o.dead, e)
		o.mu.Unlock()
		return
	}
	if e.Type == OpPut && e.BufferPath != "" && !o.keepBuffer {
		if rmErr := os.Remove(e.BufferPath); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Printf("oplog: evict buffer %s: %v", e.BufferPath, rmErr)
		}
	}
	o.markClean(e)
	log.Printf("oplog: replayed %s", e)
}

// Dead returns a snapshot of entries that failed during replay. Used
// by tests; production callers would consume + clear via a dedicated
// method.
func (o *Oplog) Dead() []OplogEntry {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]OplogEntry, len(o.dead))
	copy(out, o.dead)
	return out
}

// Bootstrap was the mount-time pre-fetch that warmed the local buffer
// from the remote store. It's intentionally removed: the new model is
// "local buffer holds writes only, reads consult the remote each time"
// (see [Config.Remote] and the fusefs Lookup/Attr/ReadDirAll/Open
// handlers). Pre-fetching would re-introduce the stale-cache problem
// that switch was meant to solve.
