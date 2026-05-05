package fusefs

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sandbox-platform/agent-sandbox/internal/remotefs"
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
// follow-up will add disk-spilling + retry-with-backoff. For the
// prototype's purposes the in-memory list is enough to surface upload
// failures during tests.
type Oplog struct {
	store remotefs.Store
	queue chan OplogEntry

	mu   sync.Mutex
	dead []OplogEntry
}

// NewOplog returns an Oplog that will replay to store. depth is the
// channel buffer; an Enqueue on a full queue blocks the FUSE handler.
func NewOplog(store remotefs.Store, depth int) *Oplog {
	return &Oplog{
		store: store,
		queue: make(chan OplogEntry, depth),
	}
}

// Enqueue submits an entry to the uploader. Blocks if the queue is full.
func (o *Oplog) Enqueue(e OplogEntry) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	o.queue <- e
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
func (o *Oplog) flush(ctx context.Context, e OplogEntry) {
	var err error
	switch e.Type {
	case OpPut:
		var f *os.File
		f, err = os.Open(e.BufferPath)
		if err == nil {
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

// Bootstrap walks the remote store at mount time and copies any
// objects missing from the local buffer down into it. The result is a
// hot read path: every Get the agent does after this returns from the
// local buffer with no network call.
//
// Existing local files are NOT overwritten — that lets sandboxd's
// agent-rootfs seed run before bootstrap, with seed content winning
// any conflict.
func Bootstrap(ctx context.Context, store remotefs.Store, bufferDir, mountPoint string) error {
	paths, err := store.List(ctx, "")
	if err != nil {
		return fmt.Errorf("bootstrap: list: %w", err)
	}
	for _, p := range paths {
		// Strip mountPoint from the remote-side path so we get a
		// buffer-relative location: remote keys are full agent-visible
		// paths (e.g. "/workspace/inputs/data.txt") and the local
		// buffer is rooted at the corresponding fs.backend directory.
		rel := stripPrefix(p, mountPoint)
		local := filepath.Join(bufferDir, rel)
		if _, err := os.Stat(local); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return fmt.Errorf("bootstrap: mkdir %s: %w", local, err)
		}
		rc, err := store.Get(ctx, p)
		if err != nil {
			log.Printf("bootstrap: skip %s: %v", p, err)
			continue
		}
		f, err := os.Create(local)
		if err != nil {
			rc.Close()
			return fmt.Errorf("bootstrap: create %s: %w", local, err)
		}
		if _, err := io.Copy(f, rc); err != nil {
			f.Close()
			rc.Close()
			return fmt.Errorf("bootstrap: copy %s: %w", local, err)
		}
		f.Close()
		rc.Close()
	}
	return nil
}

func stripPrefix(p, prefix string) string {
	if len(p) >= len(prefix) && p[:len(prefix)] == prefix {
		return p[len(prefix):]
	}
	return p
}
