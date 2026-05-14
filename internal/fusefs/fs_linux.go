//go:build linux

package fusefs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"bazil.org/fuse"
	bazilfs "bazil.org/fuse/fs"

	"github.com/sandbox-platform/agent-sandbox/internal/remotefs"
)

// Config drives a [Server]. MountPoint is where the FUSE filesystem appears
// to the agent (e.g. "/workspace"); Backend is the host directory whose
// contents the agent sees through the mount.
//
// MountPath in ACL rules is the *agent-visible* path (rooted at /, where /
// is the mount root). E.g., a rule {"/secret/**", "deny"} denies access to
// MountPoint+"/secret/...".
type Config struct {
	MountPoint string
	Backend    string
	ACLs       *ACLs
	Audit      io.Writer
	// Oplog, when non-nil, receives an [OplogEntry] after every
	// successful mutation (Create, Write, Remove, Rename). The
	// uploader goroutine started by [Server.Serve] drains it into
	// the configured remote store; on successful flush the local
	// buffer file is evicted so [Backend] holds only pending writes.
	Oplog *Oplog
	// Remote, when non-nil, is the upstream store consulted for
	// every read-side operation (Lookup, Attr, ReadDirAll, Open).
	// The agent always sees the latest upstream state — [Backend]
	// is reduced to a write buffer for in-flight Puts. Leave nil
	// for local-only mounts (no upstream).
	Remote remotefs.Store
	// RemoteStatTTL controls how long ReadDirAll-populated metadata
	// stays cached for follow-up Attr/Lookup calls. The motivating
	// pattern is `ls -la <dir>`: kernel issues 1 readdir + N attrs,
	// and without the cache each attr is its own Remote.Stat round-
	// trip. The cache is consulted only when the path is not dirty
	// (pending oplog writes always defer to the local buffer), and
	// invalidated by every mutating handler. Zero defaults to
	// [defaultRemoteStatTTL]; negative disables the cache.
	RemoteStatTTL time.Duration
}

// defaultRemoteStatTTL is the cache window used when Config.RemoteStatTTL
// is unset. Long enough to coalesce a back-to-back-`ls` workflow into a
// single Drive call; short enough that out-of-band Drive edits surface
// within a coffee-sip.
const defaultRemoteStatTTL = 30 * time.Second

// AuditEvent is one record on the audit.filesystem topic (DESIGN.md §9.1).
//
// DurationMs is the wall-clock time spent in the backend op for the
// allow/error verdicts. Zero on `deny` (ACL short-circuit; the backend
// was never touched). Consumers split a single event into fs.request
// (always) + fs.response (verdict in {allow, error}) — duration lives
// on the response side.
type AuditEvent struct {
	At         time.Time `json:"at"`
	Type       string    `json:"type"` // "filesystem"
	Op         string    `json:"op"`
	Path       string    `json:"path"`
	Verdict    string    `json:"verdict"` // "allow" | "deny" | "error"
	DurationMs int       `json:"duration_ms,omitempty"`
	Err        string    `json:"err,omitempty"`
}

// Server holds a running FUSE mount.
type Server struct {
	cfg  Config
	conn *fuse.Conn

	auditMu  sync.Mutex
	auditEnc *json.Encoder

	// statCache memoizes Remote.Stat results within RemoteStatTTL so a
	// readdir-followed-by-N-stats pattern is one API call instead of N+1.
	// Nil for pure-local mounts.
	statCache *statCache
}

// Mount opens the FUSE connection and mounts at cfg.MountPoint.
// Caller must defer [Server.Unmount].
func Mount(cfg Config) (*Server, error) {
	if cfg.MountPoint == "" || cfg.Backend == "" {
		return nil, errors.New("fusefs: MountPoint and Backend required")
	}
	if cfg.ACLs == nil {
		cfg.ACLs = Compile(nil) // default deny everywhere
	}
	if cfg.Audit == nil {
		return nil, errors.New("fusefs: Audit sink required")
	}
	c, err := fuse.Mount(cfg.MountPoint,
		fuse.FSName("sbxfuse"),
		fuse.Subtype("sbx"),
		fuse.AllowOther(),
	)
	if err != nil {
		return nil, fmt.Errorf("fusefs: mount %s: %w", cfg.MountPoint, err)
	}
	s := &Server{cfg: cfg, conn: c, auditEnc: json.NewEncoder(cfg.Audit)}
	if cfg.Remote != nil {
		ttl := cfg.RemoteStatTTL
		if ttl == 0 {
			ttl = defaultRemoteStatTTL
		}
		s.statCache = newStatCache(ttl)
	}
	return s, nil
}

// Serve handles FUSE requests until the mount is unmounted or ctx is
// cancelled. If an Oplog is configured, its uploader goroutine runs
// alongside the FUSE server; on shutdown Serve waits for the oplog to
// drain pending entries before returning, so a SIGTERM doesn't lose
// writes the agent already considers committed (each FUSE Write
// returned success the moment its buffer write landed locally; the
// remote upload happens on the oplog goroutine).
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.Unmount()
	}()
	oplogDone := make(chan struct{})
	if s.cfg.Oplog != nil {
		go func() {
			defer close(oplogDone)
			s.cfg.Oplog.Run(ctx)
		}()
	} else {
		close(oplogDone)
	}
	err := bazilfs.Serve(s.conn, &fileSystem{s: s})
	<-oplogDone
	return err
}

// Unmount releases the FUSE mount.
func (s *Server) Unmount() error {
	_ = fuse.Unmount(s.cfg.MountPoint)
	return s.conn.Close()
}

func (s *Server) audit(e AuditEvent) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	_ = s.auditEnc.Encode(e)
}

// auditDone stamps wall-clock duration (since `start`) onto `e` and
// emits it. Used by handlers on the allow/error paths where the
// backend op actually ran; deny short-circuits before the backend and
// stays on plain audit().
func (s *Server) auditDone(start time.Time, e AuditEvent) {
	e.DurationMs = int(time.Since(start) / time.Millisecond)
	s.audit(e)
}

// cachePut stores remote metadata in the stat cache, but only when the
// path is not currently the target of a pending oplog write. Skipping
// dirty paths is what keeps the cache safe across the post-flush race:
// an in-flight upload can't leave behind a stale pre-write snapshot
// that survives [Oplog.IsDirty] flipping back to false on completion,
// because we never cached during the dirty window in the first place.
func (s *Server) cachePut(p string, info remotefs.FileInfo) {
	if s.cfg.Oplog != nil && s.cfg.Oplog.IsDirty(p) {
		return
	}
	s.statCache.put(p, info)
}

// enqueuePut / enqueueDelete / enqueueMove are no-ops when no journal
// is configured. They run on the FUSE handler's goroutine; if the
// queue is full Enqueue blocks, applying back-pressure to the agent.
func (s *Server) enqueuePut(absPath, bufferPath string) {
	if s.cfg.Oplog == nil {
		return
	}
	s.cfg.Oplog.Enqueue(OplogEntry{Type: OpPut, Path: absPath, BufferPath: bufferPath})
}

func (s *Server) enqueueDelete(absPath string) {
	if s.cfg.Oplog == nil {
		return
	}
	s.cfg.Oplog.Enqueue(OplogEntry{Type: OpDelete, Path: absPath})
}

func (s *Server) enqueueMove(srcAbs, dstAbs string) {
	if s.cfg.Oplog == nil {
		return
	}
	s.cfg.Oplog.Enqueue(OplogEntry{Type: OpMove, Path: srcAbs, NewPath: dstAbs})
}

// fileSystem is the bazil/fuse FS impl.
type fileSystem struct{ s *Server }

func (f *fileSystem) Root() (bazilfs.Node, error) {
	return &node{s: f.s, virtPath: "/"}, nil
}

// node represents a FUSE node — a directory or file. virtPath is the
// agent-visible path (rooted at /); hostPath is computed by joining the
// backend.
type node struct {
	s        *Server
	virtPath string
}

func (n *node) hostPath() string {
	rel := path.Clean(n.virtPath)
	rel = filepath.FromSlash(rel)
	return filepath.Join(n.s.cfg.Backend, filepath.Clean(string(filepath.Separator)+rel))
}

// absPath returns the agent-visible absolute path: the mount point
// prefixed onto virtPath. This is what ACL rules in spec.yaml are
// expressed against (e.g. "/workspace/secret/**") and what audit
// events surface so the path matches what the agent itself sees.
func (n *node) absPath() string {
	return path.Clean(n.s.cfg.MountPoint + "/" + n.virtPath)
}

func (n *node) access() Access {
	return n.s.cfg.ACLs.Eval(n.absPath())
}

// childAbs returns the agent-visible absolute path of a child file
// without materializing a node — used by Lookup, Remove, Mkdir,
// Create, Rename for ACL evaluation + audit on a path that doesn't
// have its own node yet.
func (n *node) childAbs(name string) string {
	return path.Clean(n.absPath() + "/" + name)
}

// Attr fills the node's attributes.
//
// Resolution order — pick the freshest source for the agent:
//  1. Local Lstat — only authoritative when the path is dirty (a write
//     queued in the oplog hasn't reached upstream yet, so the local
//     buffer holds the truth).
//  2. Remote Stat — for everything else, even if a local file happens
//     to exist (it's a leftover read fetch, treat it as cache).
//  3. Local Lstat fallback — when there's no remote configured (pure
//     local backend), or the remote call errors transiently.
func (n *node) Attr(ctx context.Context, a *fuse.Attr) error {
	start := time.Now()
	if n.access() == AccessDeny {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "attr", Path: n.absPath(), Verdict: "deny"})
		return syscall.ENOENT
	}
	if n.s.cfg.Remote != nil && !n.isDirty() {
		if info, ok := n.s.statCache.get(n.absPath()); ok {
			fillAttrFromRemote(a, info)
			n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "attr", Path: n.absPath(), Verdict: "allow"})
			return nil
		}
		info, err := n.s.cfg.Remote.Stat(ctx, n.absPath())
		if err == nil {
			n.s.cachePut(n.absPath(), info)
			fillAttrFromRemote(a, info)
			n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "attr", Path: n.absPath(), Verdict: "allow"})
			return nil
		}
		// Remote ErrNotExist or transient failure → fall through to
		// local Lstat. The local fallback is what makes "Create then
		// Attr" work for a brand-new file: Create writes a local stub
		// without enqueueing (avoiding the double-enqueue race with
		// Write), and the next Attr finds the stub here.
	}
	st, err := os.Lstat(n.hostPath())
	if err != nil {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "attr", Path: n.absPath(), Verdict: "error", Err: err.Error()})
		return mapErr(err)
	}
	fillAttr(a, st)
	n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "attr", Path: n.absPath(), Verdict: "allow"})
	return nil
}

// isDirty is true when a write to this node's path is queued or in
// flight in the oplog. Used by read-side handlers to choose "serve
// local buffer" over "fetch from remote": the buffer holds the latest
// data the agent itself wrote and the remote doesn't know about it yet.
func (n *node) isDirty() bool {
	if n.s.cfg.Oplog == nil {
		return false
	}
	return n.s.cfg.Oplog.IsDirty(n.absPath())
}

// Lookup resolves a child by name.
//
// For remote-backed mounts the existence check is Remote.Stat (or
// local Lstat when the child is dirty). We never invent a node from
// thin air — if neither source confirms the child exists, return
// ENOENT so the kernel doesn't cache a phantom inode.
func (n *node) Lookup(ctx context.Context, name string) (bazilfs.Node, error) {
	start := time.Now()
	child := &node{s: n.s, virtPath: path.Join(n.virtPath, name)}
	if child.access() == AccessDeny {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "lookup", Path: child.absPath(), Verdict: "deny"})
		return nil, syscall.ENOENT
	}
	if n.s.cfg.Remote != nil && !child.isDirty() {
		if _, ok := n.s.statCache.get(child.absPath()); ok {
			n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "lookup", Path: child.absPath(), Verdict: "allow"})
			return child, nil
		}
		info, err := n.s.cfg.Remote.Stat(ctx, child.absPath())
		if err == nil {
			n.s.cachePut(child.absPath(), info)
			n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "lookup", Path: child.absPath(), Verdict: "allow"})
			return child, nil
		}
		// Remote ErrNotExist or transient failure → fall through to local
		// Lstat. This is how a freshly-Create'd file (no enqueue, no
		// remote presence yet) becomes lookup-able. ENOENT is only the
		// final answer when both sides come back empty.
	}
	if _, err := os.Lstat(child.hostPath()); err != nil {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "lookup", Path: child.absPath(), Verdict: "error", Err: err.Error()})
		return nil, mapErr(err)
	}
	n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "lookup", Path: child.absPath(), Verdict: "allow"})
	return child, nil
}

// ReadDirAll lists the directory.
//
// For remote-backed mounts the canonical listing comes from Remote.ListDir;
// any locally-buffered children (in-flight writes) are merged on top so
// the agent sees its own pending creates immediately. Pure-local mounts
// just list the backend dir directly.
func (n *node) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	start := time.Now()
	if n.access() == AccessDeny {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "readdir", Path: n.absPath(), Verdict: "deny"})
		return nil, syscall.ENOENT
	}
	seen := map[string]fuse.DirentType{}

	if n.s.cfg.Remote != nil {
		infos, err := n.s.cfg.Remote.ListDir(ctx, n.absPath())
		if err != nil && !errors.Is(err, remotefs.ErrNotExist) {
			n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "readdir", Path: n.absPath(), Verdict: "error", Err: err.Error()})
			return nil, mapErr(err)
		}
		// Cache the parent's own FileInfo so a follow-up Attr on this
		// dir is a hit. We don't have the directory's own remote
		// metadata here (ListDir returns children, not parent
		// attrs); synthesize a minimal IsDir=true entry — the kernel
		// only ever uses IsDir + Mtime for directories on the read path,
		// and the cache TTL bounds how long the synthesized mtime is
		// served before a real Stat refreshes it.
		n.s.cachePut(n.absPath(), remotefs.FileInfo{Path: n.absPath(), IsDir: true, Mtime: time.Now()})
		for _, info := range infos {
			name := path.Base(info.Path)
			childAbs := n.childAbs(name)
			if n.s.cfg.ACLs.Eval(childAbs) == AccessDeny {
				continue
			}
			t := fuse.DT_File
			if info.IsDir {
				t = fuse.DT_Dir
			}
			seen[name] = t
			// Populate the stat cache so the kernel's follow-up Attr
			// fan-out (one per entry, immediate after readdir) reuses
			// metadata we already paid for in this ListDir call. Skips
			// dirty children — those serve from local Lstat anyway.
			n.s.cachePut(childAbs, info)
		}
	}

	// Merge in any local children. For a remote-backed mount these are
	// pending writes (oplog hasn't flushed yet); for a local-only mount
	// they're the only source of truth. Stat errors on individual
	// entries are skipped, not fatal — a transient read race shouldn't
	// blank the entire listing.
	if entries, err := os.ReadDir(n.hostPath()); err == nil {
		for _, e := range entries {
			if n.s.cfg.ACLs.Eval(n.childAbs(e.Name())) == AccessDeny {
				continue
			}
			t := fuse.DT_File
			if e.IsDir() {
				t = fuse.DT_Dir
			}
			seen[e.Name()] = t
		}
	} else if n.s.cfg.Remote == nil {
		// Pure-local mount and the dir doesn't exist → return the error.
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "readdir", Path: n.absPath(), Verdict: "error", Err: err.Error()})
		return nil, mapErr(err)
	}

	out := make([]fuse.Dirent, 0, len(seen))
	for name, t := range seen {
		out = append(out, fuse.Dirent{Name: name, Type: t})
	}
	n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "readdir", Path: n.absPath(), Verdict: "allow"})
	return out, nil
}

// Open opens a file or directory. We return the same node as the handle,
// so reads/writes route back through Read/Write below.
//
// For read access on a remote-backed mount, fetch the latest contents
// from the remote into the local buffer first — this is the moment the
// "always sees latest upstream" invariant gets enforced. We skip the
// fetch when:
//   - There's no remote (pure-local mount).
//   - The path is dirty (our own pending write hasn't uploaded yet,
//     so the buffer has the freshest copy).
//   - The open is write-only / truncating (the agent is about to
//     overwrite anyway; downloading would just be wasted bytes).
func (n *node) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (bazilfs.Handle, error) {
	start := time.Now()
	verdict := n.access()
	if verdict == AccessDeny {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "open", Path: n.absPath(), Verdict: "deny"})
		return nil, syscall.ENOENT
	}
	if verdict == AccessRO && (req.Flags&fuse.OpenWriteOnly != 0 || req.Flags&fuse.OpenReadWrite != 0) {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "open-write", Path: n.absPath(), Verdict: "deny"})
		return nil, syscall.EROFS
	}
	if n.s.cfg.Remote != nil && !n.isDirty() {
		// Directory opens (req.Dir = OPENDIR) don't need a remote Stat:
		// the kernel only OPENDIRs a node it already knows is a dir
		// (via Lookup/Attr), and our only setup work is MkdirAll on the
		// local buffer so ReadDirAll's local-merge step has somewhere
		// to look. Skipping the Stat is what gets a repeat `ls <dir>`
		// down to one Drive call (the ListDir).
		if req.Dir {
			if err := os.MkdirAll(n.hostPath(), 0o755); err != nil {
				n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "open", Path: n.absPath(), Verdict: "error", Err: err.Error()})
				return nil, mapErr(err)
			}
			n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "open", Path: n.absPath(), Verdict: "allow"})
			return n, nil
		}
		err := n.materializeLocal(ctx, req.Flags)
		if err != nil {
			if !errors.Is(err, remotefs.ErrNotExist) {
				n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "open", Path: n.absPath(), Verdict: "error", Err: err.Error()})
				return nil, mapErr(err)
			}
			// Remote says the file doesn't exist. Two real cases:
			//   a) the file was just Create'd and lives only in the
			//      local buffer (not yet enqueued for upload) — local
			//      Lstat will find it; Open succeeds and serves local.
			//   b) the file is genuinely gone (rename moved it away,
			//      or another sandbox/Drive-side actor deleted it) —
			//      local Lstat will also fail; we must surface ENOENT
			//      so the kernel doesn't keep serving reads against a
			//      stale node whose dentry was aliased onto another
			//      name by a recent Rename.
			if _, statErr := os.Lstat(n.hostPath()); statErr != nil {
				n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "open", Path: n.absPath(), Verdict: "error", Err: "not found"})
				return nil, syscall.ENOENT
			}
		}
	}
	n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "open", Path: n.absPath(), Verdict: "allow"})
	return n, nil
}

// materializeLocal makes sure the local buffer holds whatever the
// subsequent FUSE handlers will need — without ever caching stale
// content for a future read. Three cases:
//
//  1. Path is a directory on the remote → MkdirAll the local placeholder
//     so ReadDirAll's local-merge step has somewhere to look. We never
//     try to Get a folder (Drive returns an error for that).
//  2. Path is a file and the open is write-only or truncating → create
//     an empty local file. The agent is about to overwrite, so fetching
//     content would be wasted bytes.
//  3. Path is a file and the open intends to read → fetch the latest
//     content from the remote into the local file via a temp + rename
//     so a partial fetch never leaves a half-file the agent could read.
//
// Returns [remotefs.ErrNotExist] when the remote has no such path —
// the caller maps that to ENOENT (open of a non-existent file with no
// O_CREAT, which Lookup should already have caught, but defence in depth).
func (n *node) materializeLocal(ctx context.Context, flags fuse.OpenFlags) error {
	// Try the stat cache first — Attr/Lookup just before this Open
	// commonly populated it, so re-fetching the same metadata over the
	// wire is wasted work. On miss, populate the cache so a follow-up
	// Attr in the same TTL window also stays local.
	info, ok := n.s.statCache.get(n.absPath())
	if !ok {
		var err error
		info, err = n.s.cfg.Remote.Stat(ctx, n.absPath())
		if err != nil {
			return err
		}
		n.s.cachePut(n.absPath(), info)
	}
	host := n.hostPath()
	if info.IsDir {
		return os.MkdirAll(host, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		return err
	}
	// O_TRUNC is the ONLY flag that means "agent is about to overwrite
	// from byte 0 — no existing content needed." Plain O_WRONLY,
	// O_APPEND, and O_RDWR all want to see the current bytes (the
	// agent might seek + patch, append, or read-modify-write). For
	// those, fall through to the Get path so the local buffer holds
	// the upstream content before the first Write.
	if flags&fuse.OpenTruncate != 0 {
		f, err := os.Create(host)
		if err != nil {
			return err
		}
		return f.Close()
	}
	rc, err := n.s.cfg.Remote.Get(ctx, n.absPath())
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(filepath.Dir(host), ".sbxfuse-fetch-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), host)
}

// Read returns file bytes at the requested offset.
func (n *node) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	start := time.Now()
	if n.access() == AccessDeny {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "read", Path: n.absPath(), Verdict: "deny"})
		return syscall.ENOENT
	}
	f, err := os.Open(n.hostPath())
	if err != nil {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "read", Path: n.absPath(), Verdict: "error", Err: err.Error()})
		return mapErr(err)
	}
	defer f.Close()
	buf := make([]byte, req.Size)
	nRead, err := f.ReadAt(buf, req.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "read", Path: n.absPath(), Verdict: "error", Err: err.Error()})
		return mapErr(err)
	}
	resp.Data = buf[:nRead]
	n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "read", Path: n.absPath(), Verdict: "allow"})
	return nil
}

// Write writes file bytes at the requested offset.
func (n *node) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	start := time.Now()
	if n.access() != AccessRW {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "write", Path: n.absPath(), Verdict: "deny"})
		return syscall.EROFS
	}
	f, err := os.OpenFile(n.hostPath(), os.O_WRONLY, 0)
	if err != nil {
		return mapErr(err)
	}
	defer f.Close()
	nWritten, err := f.WriteAt(req.Data, req.Offset)
	if err != nil {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "write", Path: n.absPath(), Verdict: "error", Err: err.Error()})
		return mapErr(err)
	}
	resp.Size = nWritten
	n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "write", Path: n.absPath(), Verdict: "allow"})
	n.s.statCache.invalidate(n.absPath())
	n.s.enqueuePut(n.absPath(), n.hostPath())
	return nil
}

// Create creates a new file inside this directory.
//
// Parent dirs are auto-created in the local buffer because we no longer
// pre-populate the directory hierarchy at mount time (Bootstrap is
// gone — the local buffer holds writes only). The remote-side parent
// hierarchy is created lazily by the Store on Put.
func (n *node) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (bazilfs.Node, bazilfs.Handle, error) {
	start := time.Now()
	child := &node{s: n.s, virtPath: path.Join(n.virtPath, req.Name)}
	if child.access() != AccessRW {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "create", Path: child.absPath(), Verdict: "deny"})
		return nil, nil, syscall.EROFS
	}
	if err := os.MkdirAll(filepath.Dir(child.hostPath()), 0o755); err != nil {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "create", Path: child.absPath(), Verdict: "error", Err: err.Error()})
		return nil, nil, mapErr(err)
	}
	f, err := os.OpenFile(child.hostPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "create", Path: child.absPath(), Verdict: "error", Err: err.Error()})
		return nil, nil, mapErr(err)
	}
	_ = f.Close()
	n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "create", Path: child.absPath(), Verdict: "allow"})
	n.s.statCache.invalidate(child.absPath())
	// We deliberately do NOT enqueue a Put here. The common
	// "open(O_CREAT|O_TRUNC) + Write + Close" sequence would double-
	// enqueue (once empty from Create, once with content from Write),
	// and the empty Put can race ahead, evict the buffer, and starve
	// the content Put — losing the agent's write. Write enqueues with
	// the right content; an empty `touch` is left out of scope until
	// we add a Flush/Release-time enqueue (see fusefs TODO).
	return child, child, nil
}

// Remove unlinks a file or empty directory.
//
// Two paths because the file may live only on the remote (no local
// buffer copy after eviction):
//
//   - Pure-local mount or local copy present: os.Remove + (if dirty)
//     enqueue OpDelete behind the pending OpPut so the queue's FIFO
//     order delivers the Delete after the Put lands on the remote.
//   - Remote-only file: synchronous Remote.Delete so the agent's next
//     Lookup correctly returns ENOENT. Async would race the read-back.
//
// "Neither local nor remote" → ENOENT, matching POSIX `rm` semantics.
func (n *node) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	start := time.Now()
	childAbs := n.childAbs(req.Name)
	if n.s.cfg.ACLs.Eval(childAbs) != AccessRW {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "remove", Path: childAbs, Verdict: "deny"})
		return syscall.EROFS
	}
	hostChild := filepath.Join(n.hostPath(), req.Name)
	localErr := os.Remove(hostChild)
	if localErr != nil && !os.IsNotExist(localErr) {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "remove", Path: childAbs, Verdict: "error", Err: localErr.Error()})
		return mapErr(localErr)
	}
	localExisted := localErr == nil
	// Whether the local unlink succeeded or the file was already gone,
	// the path's cached stat (if any) is now stale.
	n.s.statCache.invalidate(childAbs)

	if n.s.cfg.Remote != nil {
		dirty := n.s.cfg.Oplog != nil && n.s.cfg.Oplog.IsDirty(childAbs)
		if dirty {
			// A pending OpPut for this path is queued or in flight.
			// Enqueue OpDelete so the FIFO queue runs Put-then-Delete
			// against the remote (the wasted upload is cheaper than
			// stalling the FUSE handler waiting for the Put to finish).
			n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "remove", Path: childAbs, Verdict: "allow"})
			n.s.enqueueDelete(childAbs)
			return nil
		}
		if err := n.s.cfg.Remote.Delete(ctx, childAbs); err != nil && !errors.Is(err, remotefs.ErrNotExist) {
			n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "remove", Path: childAbs, Verdict: "error", Err: err.Error()})
			return mapErr(err)
		}
	} else if !localExisted {
		// Pure-local mount and the file never existed.
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "remove", Path: childAbs, Verdict: "error", Err: "not found"})
		return syscall.ENOENT
	}
	n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "remove", Path: childAbs, Verdict: "allow"})
	return nil
}

// Mkdir creates a subdirectory.
func (n *node) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (bazilfs.Node, error) {
	start := time.Now()
	child := &node{s: n.s, virtPath: path.Join(n.virtPath, req.Name)}
	if child.access() != AccessRW {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "mkdir", Path: child.absPath(), Verdict: "deny"})
		return nil, syscall.EROFS
	}
	if err := os.Mkdir(child.hostPath(), 0o755); err != nil {
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "mkdir", Path: child.absPath(), Verdict: "error", Err: err.Error()})
		return nil, mapErr(err)
	}
	n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "mkdir", Path: child.absPath(), Verdict: "allow"})
	n.s.statCache.invalidate(child.absPath())
	return child, nil
}

// Rename moves a child of n to newDir under a new name. Both endpoints
// must have rw access — the rule trie is consulted for the source
// (preventing exfiltration of a deny-listed file via rename out of its
// directory) and for the destination (preventing a write into a
// deny-listed location). Auditing emits one event with both paths.
//
// Like Remove, Rename has to handle remote-only sources: for a file
// that's been evicted from the local buffer the remote rename is
// synchronous so the agent's next Lookup on the new name succeeds.
// For a dirty source (pending OpPut), we enqueue OpMove behind the
// Put — the FIFO queue keeps Put→Move ordered against the remote.
func (n *node) Rename(ctx context.Context, req *fuse.RenameRequest, newDir bazilfs.Node) error {
	start := time.Now()
	dst, ok := newDir.(*node)
	if !ok {
		return syscall.EXDEV
	}
	oldAbs := n.childAbs(req.OldName)
	newAbs := dst.childAbs(req.NewName)
	if n.s.cfg.ACLs.Eval(oldAbs) != AccessRW || n.s.cfg.ACLs.Eval(newAbs) != AccessRW {
		n.s.auditDone(start, AuditEvent{
			At: time.Now(), Type: "filesystem", Op: "rename",
			Path: oldAbs + " → " + newAbs, Verdict: "deny",
		})
		return syscall.EROFS
	}
	oldHost := filepath.Join(n.hostPath(), req.OldName)
	newHost := filepath.Join(dst.hostPath(), req.NewName)
	// Make sure the destination's parent dir exists locally — needed
	// when the destination is a remote-only path (Bootstrap is gone,
	// so subdirs only materialize on demand).
	if err := os.MkdirAll(filepath.Dir(newHost), 0o755); err != nil {
		n.s.auditDone(start, AuditEvent{
			At: time.Now(), Type: "filesystem", Op: "rename",
			Path: oldAbs + " → " + newAbs, Verdict: "error", Err: err.Error(),
		})
		return mapErr(err)
	}
	localErr := os.Rename(oldHost, newHost)
	if localErr != nil && !os.IsNotExist(localErr) {
		n.s.auditDone(start, AuditEvent{
			At: time.Now(), Type: "filesystem", Op: "rename",
			Path: oldAbs + " → " + newAbs, Verdict: "error", Err: localErr.Error(),
		})
		return mapErr(localErr)
	}
	localRenamed := localErr == nil
	// Drop both ends from the stat cache: the old name is gone, the
	// new name's metadata changed underneath any prior cached entry.
	n.s.statCache.invalidate(oldAbs)
	n.s.statCache.invalidate(newAbs)

	if n.s.cfg.Remote != nil {
		dirty := n.s.cfg.Oplog != nil && n.s.cfg.Oplog.IsDirty(oldAbs)
		if dirty {
			n.s.auditDone(start, AuditEvent{
				At: time.Now(), Type: "filesystem", Op: "rename",
				Path: oldAbs + " → " + newAbs, Verdict: "allow",
			})
			n.s.enqueueMove(oldAbs, newAbs)
			return nil
		}
		if err := n.s.cfg.Remote.Move(ctx, oldAbs, newAbs); err != nil {
			// Try to undo a local rename so the agent's view stays
			// consistent with the remote (which is the source of truth).
			if localRenamed {
				_ = os.Rename(newHost, oldHost)
			}
			if errors.Is(err, remotefs.ErrNotExist) && !localRenamed {
				n.s.auditDone(start, AuditEvent{
					At: time.Now(), Type: "filesystem", Op: "rename",
					Path: oldAbs + " → " + newAbs, Verdict: "error", Err: "source not found",
				})
				return syscall.ENOENT
			}
			n.s.auditDone(start, AuditEvent{
				At: time.Now(), Type: "filesystem", Op: "rename",
				Path: oldAbs + " → " + newAbs, Verdict: "error", Err: err.Error(),
			})
			return mapErr(err)
		}
	} else if !localRenamed {
		n.s.auditDone(start, AuditEvent{
			At: time.Now(), Type: "filesystem", Op: "rename",
			Path: oldAbs + " → " + newAbs, Verdict: "error", Err: "not found",
		})
		return syscall.ENOENT
	}
	n.s.auditDone(start, AuditEvent{
		At: time.Now(), Type: "filesystem", Op: "rename",
		Path: oldAbs + " → " + newAbs, Verdict: "allow",
	})
	return nil
}

// Fsync is a no-op (we write through to the host file).
func (n *node) Fsync(ctx context.Context, req *fuse.FsyncRequest) error { return nil }

// Setattr handles attribute mutations the kernel asks for. The one
// we *must* implement correctly is truncate — without it, an
// `open(O_TRUNC)` (or an explicit `ftruncate(0)` from `echo > file`)
// would silently no-op, and a subsequent `>>` append would see no
// difference from `>`. The kernel issues SETATTR(size=N) for those.
//
// Other Setattr fields (mode, uid, gid, atimes) are accepted as
// no-ops — the FUSE ACL is the access boundary, POSIX bits don't
// need to be authoritative.
func (n *node) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	start := time.Now()
	if req.Valid.Size() {
		if n.access() != AccessRW {
			n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "truncate", Path: n.absPath(), Verdict: "deny"})
			return syscall.EROFS
		}
		host := n.hostPath()
		// Materialize a local file if the path lives only on the
		// remote — Truncate on a missing file returns ENOENT, but the
		// agent's intent is "this file should be size N", which we can
		// satisfy by creating a fresh local stub of that size.
		if _, err := os.Stat(host); err != nil {
			if !os.IsNotExist(err) {
				n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "truncate", Path: n.absPath(), Verdict: "error", Err: err.Error()})
				return mapErr(err)
			}
			if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
				n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "truncate", Path: n.absPath(), Verdict: "error", Err: err.Error()})
				return mapErr(err)
			}
			f, err := os.Create(host)
			if err != nil {
				n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "truncate", Path: n.absPath(), Verdict: "error", Err: err.Error()})
				return mapErr(err)
			}
			f.Close()
		}
		if err := os.Truncate(host, int64(req.Size)); err != nil {
			n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "truncate", Path: n.absPath(), Verdict: "error", Err: err.Error()})
			return mapErr(err)
		}
		n.s.auditDone(start, AuditEvent{At: time.Now(), Type: "filesystem", Op: "truncate", Path: n.absPath(), Verdict: "allow"})
		n.s.statCache.invalidate(n.absPath())
		// Don't enqueue here — Write will, and a typical truncate is
		// followed by a Write. A bare truncate-no-write leaves the
		// local stub un-uploaded (same edge case as bare Create).
	}
	return n.Attr(ctx, &resp.Attr)
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, os.ErrPermission):
		return syscall.EACCES
	default:
		return err
	}
}

func fillAttr(a *fuse.Attr, st os.FileInfo) {
	a.Size = uint64(st.Size())
	a.Mode = st.Mode()
	a.Mtime = st.ModTime()
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		a.Inode = sys.Ino
		a.Nlink = uint32(sys.Nlink)
		a.Uid = sys.Uid
		a.Gid = sys.Gid
	}
	a.Valid = 0 // see noKernelAttrCache below
}

// fillAttrFromRemote fills Attr from a remotefs.FileInfo. We don't have
// owner/inode info from the remote, so we leave the kernel to assign an
// inode and report root-owned, world-readable permissions — the agent
// runs as root inside the sandbox-pod and the FUSE layer is the access
// boundary, not POSIX uid bits.
func fillAttrFromRemote(a *fuse.Attr, info remotefs.FileInfo) {
	a.Size = uint64(info.Size)
	a.Mtime = info.Mtime
	if info.IsDir {
		a.Mode = os.ModeDir | 0o755
	} else {
		a.Mode = 0o644
	}
	a.Nlink = 1
	a.Valid = 0 // see noKernelAttrCache below
}

// noKernelAttrCache: setting Attr.Valid = 0 tells bazilfs (and the
// kernel) that the returned attributes / entry are valid only for this
// one call — subsequent stats, lookups, and opens must call back into
// our handlers rather than serve from the kernel dcache. This is
// what's needed for the "always sees latest from upstream" invariant:
// without it the kernel would happily serve a stale dentry across an
// out-of-band Drive change OR across our own Rename (the kernel's
// cached old-name dentry still points at the source node, whose
// virtPath we no longer mutate after rename).
const noKernelAttrCache = 0
