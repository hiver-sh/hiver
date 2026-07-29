package fusefs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hiver-sh/hiver/internal/remotefs"
)

// SyncAuditType is the Type value carried by every [SyncAuditEvent]. It
// rides the same audit sink as the filesystem [AuditEvent]; sandboxd's
// translator switches on Type to tell the two streams apart.
const SyncAuditType = "fs-sync"

// ControlUnmountDoneType is the Type of the control-channel acknowledgement
// sbxfuse writes onto the audit sink once a mount's oplog has fully drained in
// response to an "unmount" command. It carries a "mount" field (the host mount
// point). sandboxd blocks its Unmount on this event so a mount's pending remote
// writes (e.g. a write-on-shutdown snapshot tarball) are durable before the
// mount's local write buffer is torn down. The translators ignore it — it is
// control signalling, not a sandbox-visible event.
const ControlUnmountDoneType = "control-unmount-done"

// SyncAuditEvent is one record describing a request to an external
// storage backend (a [remotefs.Store]) — e.g. a GCS Put or an S3 Get.
//
// Each backend call produces a pair of events sharing one RequestID:
//
//   - Phase="request" is emitted immediately before the backend call.
//   - Phase="response" is emitted once the call returns, carrying
//     DurationMs and an optional Err.
//
// sandboxd's translator maps the pair onto system.fs-sync-request and
// system.fs-sync-response SandboxEvents.
type SyncAuditEvent struct {
	At         time.Time `json:"at"`
	Type       string    `json:"type"`  // always SyncAuditType
	Phase      string    `json:"phase"` // "request" | "response"
	RequestID  string    `json:"request_id"`
	Op         string    `json:"op"`   // list|list-dir|stat|get|put|delete|move
	Path       string    `json:"path"` // host FUSE path (mount + store path)
	Backend    string    `json:"backend"`
	DurationMs int       `json:"duration_ms,omitempty"` // response phase
	Err        string    `json:"err,omitempty"`         // response phase
}

// auditedStore wraps a [remotefs.Store], emitting a request/response
// [SyncAuditEvent] pair around every backend call. It is transparent:
// callers see the inner store's behaviour unchanged.
//
// mount is the host FUSE mount point, prepended to the store-relative
// paths the inner store receives. sandboxd routes each event to the
// owning sandbox's broker by longest host-mount prefix — the same match
// used for filesystem audit events — so the path must be host-absolute.
type auditedStore struct {
	inner   remotefs.Store
	backend string
	mount   string
	audit   func(SyncAuditEvent)
	seq     atomic.Uint64
}

// NewAuditedStore wraps store so every backend request emits a
// system.fs-sync-request / system.fs-sync-response pair on the audit
// sink. backend is the backend name (gcs, s3, …); mount is the host FUSE
// mount point. A nil store returns nil — a local-only mount has no
// external backend and emits no sync events.
func NewAuditedStore(store remotefs.Store, backend, mount string, audit io.Writer) remotefs.Store {
	if store == nil {
		return nil
	}
	var mu sync.Mutex
	enc := json.NewEncoder(audit)
	emit := func(e SyncAuditEvent) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(e)
	}
	return &auditedStore{inner: store, backend: backend, mount: mount, audit: emit}
}

// begin emits the request-phase event for op on storePath and returns a
// closure that emits the paired response-phase event. err is reported on
// the response except for [remotefs.ErrNotExist], which is normal control
// flow (a Stat/Get probe answering "does this exist?") rather than a
// backend failure.
func (s *auditedStore) begin(op, storePath string) func(err error) {
	id := strconv.FormatUint(s.seq.Add(1), 10)
	hostPath := path.Join(s.mount, storePath)
	start := time.Now()
	s.audit(SyncAuditEvent{
		At: start, Type: SyncAuditType, Phase: "request",
		RequestID: id, Op: op, Path: hostPath, Backend: s.backend,
	})
	return func(err error) {
		ev := SyncAuditEvent{
			At: time.Now(), Type: SyncAuditType, Phase: "response",
			RequestID: id, Op: op, Path: hostPath, Backend: s.backend,
			DurationMs: int(time.Since(start) / time.Millisecond),
		}
		if err != nil && !errors.Is(err, remotefs.ErrNotExist) {
			ev.Err = err.Error()
		}
		s.audit(ev)
	}
}

func (s *auditedStore) List(ctx context.Context, prefix string) ([]string, error) {
	done := s.begin("list", prefix)
	out, err := s.inner.List(ctx, prefix)
	done(err)
	return out, err
}

func (s *auditedStore) ListDir(ctx context.Context, dir string) ([]remotefs.FileInfo, error) {
	done := s.begin("list-dir", dir)
	out, err := s.inner.ListDir(ctx, dir)
	done(err)
	return out, err
}

func (s *auditedStore) Stat(ctx context.Context, p string) (remotefs.FileInfo, error) {
	done := s.begin("stat", p)
	out, err := s.inner.Stat(ctx, p)
	done(err)
	return out, err
}

func (s *auditedStore) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	done := s.begin("get", p)
	out, err := s.inner.Get(ctx, p)
	done(err)
	return out, err
}

func (s *auditedStore) Put(ctx context.Context, p string, content io.Reader) error {
	done := s.begin("put", p)
	err := s.inner.Put(ctx, p, content)
	done(err)
	return err
}

func (s *auditedStore) Delete(ctx context.Context, p string) error {
	done := s.begin("delete", p)
	err := s.inner.Delete(ctx, p)
	done(err)
	return err
}

func (s *auditedStore) Move(ctx context.Context, src, dst string) error {
	done := s.begin("move", src)
	err := s.inner.Move(ctx, src, dst)
	done(err)
	return err
}
