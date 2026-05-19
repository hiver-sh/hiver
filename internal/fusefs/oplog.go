package fusefs

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/blasten/hive/internal/remotefs"
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

	mu    sync.Mutex
	dead  []OplogEntry
	dirty map[string]int // agent-visible path → outstanding Op count
}

// NewOplog returns an Oplog that will replay to store. depth is the
// channel buffer; an Enqueue on a full queue blocks the FUSE handler.
func NewOplog(store remotefs.Store, depth int) *Oplog {
	return &Oplog{
		store: store,
		queue: make(chan OplogEntry, depth),
		dirty: make(map[string]int),
	}
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
	o.markDirty(e)
	o.queue <- e
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
	drained := 0
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
// directly via [Config.Remote].
func (o *Oplog) flush(ctx context.Context, e OplogEntry) {
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
				//      the same path).
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
			err = o.store.Put(ctx, e.Path, f)
			f.Close()
		}
	case OpDelete:
		err = o.store.Delete(ctx, e.Path)
	case OpMove:
		err = o.store.Move(ctx, e.Path, e.NewPath)
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
	if e.Type == OpPut && e.BufferPath != "" {
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
