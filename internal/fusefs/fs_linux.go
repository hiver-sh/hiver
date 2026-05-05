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
	// AuditReads controls whether each FUSE Read request is audited.
	// Off by default because the kernel issues one Read per chunk
	// (typically 4–128 KiB) so a single user-level read of a 1 MiB
	// file can produce many events. Open is always audited.
	AuditReads bool
}

// AuditEvent is one record on the audit.filesystem topic (DESIGN.md §9.1).
type AuditEvent struct {
	At      time.Time `json:"at"`
	Type    string    `json:"type"` // "filesystem"
	Op      string    `json:"op"`
	Path    string    `json:"path"`
	Verdict string    `json:"verdict"` // "allow" | "deny" | "error"
	Err     string    `json:"err,omitempty"`
}

// Server holds a running FUSE mount.
type Server struct {
	cfg  Config
	conn *fuse.Conn

	auditMu  sync.Mutex
	auditEnc *json.Encoder
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
	return &Server{cfg: cfg, conn: c, auditEnc: json.NewEncoder(cfg.Audit)}, nil
}

// Serve handles FUSE requests until the mount is unmounted or ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.Unmount()
	}()
	return bazilfs.Serve(s.conn, &fileSystem{s: s})
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
func (n *node) Attr(ctx context.Context, a *fuse.Attr) error {
	if n.access() == AccessDeny {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "attr", Path: n.absPath(), Verdict: "deny"})
		return syscall.ENOENT
	}
	st, err := os.Lstat(n.hostPath())
	if err != nil {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "attr", Path: n.absPath(), Verdict: "error", Err: err.Error()})
		return mapErr(err)
	}
	fillAttr(a, st)
	n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "attr", Path: n.absPath(), Verdict: "allow"})
	return nil
}

// Lookup resolves a child by name.
func (n *node) Lookup(ctx context.Context, name string) (bazilfs.Node, error) {
	child := &node{s: n.s, virtPath: path.Join(n.virtPath, name)}
	if child.access() == AccessDeny {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "lookup", Path: child.absPath(), Verdict: "deny"})
		return nil, syscall.ENOENT
	}
	if _, err := os.Lstat(child.hostPath()); err != nil {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "lookup", Path: child.absPath(), Verdict: "error", Err: err.Error()})
		return nil, mapErr(err)
	}
	n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "lookup", Path: child.absPath(), Verdict: "allow"})
	return child, nil
}

// ReadDirAll lists the directory.
func (n *node) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	if n.access() == AccessDeny {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "readdir", Path: n.absPath(), Verdict: "deny"})
		return nil, syscall.ENOENT
	}
	entries, err := os.ReadDir(n.hostPath())
	if err != nil {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "readdir", Path: n.absPath(), Verdict: "error", Err: err.Error()})
		return nil, mapErr(err)
	}
	out := make([]fuse.Dirent, 0, len(entries))
	for _, e := range entries {
		// Hide entries with deny ACL: per DESIGN.md §8.2, deny → ENOENT,
		// so they shouldn't appear in directory listings either.
		if n.s.cfg.ACLs.Eval(n.childAbs(e.Name())) == AccessDeny {
			continue
		}
		t := fuse.DT_File
		if e.IsDir() {
			t = fuse.DT_Dir
		}
		out = append(out, fuse.Dirent{Name: e.Name(), Type: t})
	}
	n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "readdir", Path: n.absPath(), Verdict: "allow"})
	return out, nil
}

// Open opens a file or directory. We return the same node as the handle,
// so reads/writes route back through Read/Write below.
func (n *node) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (bazilfs.Handle, error) {
	verdict := n.access()
	if verdict == AccessDeny {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "open", Path: n.absPath(), Verdict: "deny"})
		return nil, syscall.ENOENT
	}
	if verdict == AccessRO && (req.Flags&fuse.OpenWriteOnly != 0 || req.Flags&fuse.OpenReadWrite != 0) {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "open-write", Path: n.absPath(), Verdict: "deny"})
		return nil, syscall.EROFS
	}
	n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "open", Path: n.absPath(), Verdict: "allow"})
	return n, nil
}

// Read returns file bytes at the requested offset.
//
// Per-Read auditing is opt-in (Config.AuditReads): the kernel issues one
// Read per chunk so a single user-level read of a 1 MiB file generates
// many events. Open is always audited.
func (n *node) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	if n.access() == AccessDeny {
		if n.s.cfg.AuditReads {
			n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "read", Path: n.absPath(), Verdict: "deny"})
		}
		return syscall.ENOENT
	}
	f, err := os.Open(n.hostPath())
	if err != nil {
		if n.s.cfg.AuditReads {
			n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "read", Path: n.absPath(), Verdict: "error", Err: err.Error()})
		}
		return mapErr(err)
	}
	defer f.Close()
	buf := make([]byte, req.Size)
	nRead, err := f.ReadAt(buf, req.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		if n.s.cfg.AuditReads {
			n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "read", Path: n.absPath(), Verdict: "error", Err: err.Error()})
		}
		return mapErr(err)
	}
	resp.Data = buf[:nRead]
	if n.s.cfg.AuditReads {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "read", Path: n.absPath(), Verdict: "allow"})
	}
	return nil
}

// Write writes file bytes at the requested offset.
func (n *node) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	if n.access() != AccessRW {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "write", Path: n.absPath(), Verdict: "deny"})
		return syscall.EROFS
	}
	f, err := os.OpenFile(n.hostPath(), os.O_WRONLY, 0)
	if err != nil {
		return mapErr(err)
	}
	defer f.Close()
	nWritten, err := f.WriteAt(req.Data, req.Offset)
	if err != nil {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "write", Path: n.absPath(), Verdict: "error", Err: err.Error()})
		return mapErr(err)
	}
	resp.Size = nWritten
	n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "write", Path: n.absPath(), Verdict: "allow"})
	return nil
}

// Create creates a new file inside this directory.
func (n *node) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (bazilfs.Node, bazilfs.Handle, error) {
	child := &node{s: n.s, virtPath: path.Join(n.virtPath, req.Name)}
	if child.access() != AccessRW {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "create", Path: child.absPath(), Verdict: "deny"})
		return nil, nil, syscall.EROFS
	}
	f, err := os.OpenFile(child.hostPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "create", Path: child.absPath(), Verdict: "error", Err: err.Error()})
		return nil, nil, mapErr(err)
	}
	_ = f.Close()
	n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "create", Path: child.absPath(), Verdict: "allow"})
	return child, child, nil
}

// Remove unlinks a file or empty directory.
func (n *node) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	childAbs := n.childAbs(req.Name)
	if n.s.cfg.ACLs.Eval(childAbs) != AccessRW {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "remove", Path: childAbs, Verdict: "deny"})
		return syscall.EROFS
	}
	hostChild := filepath.Join(n.hostPath(), req.Name)
	if err := os.Remove(hostChild); err != nil {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "remove", Path: childAbs, Verdict: "error", Err: err.Error()})
		return mapErr(err)
	}
	n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "remove", Path: childAbs, Verdict: "allow"})
	return nil
}

// Mkdir creates a subdirectory.
func (n *node) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (bazilfs.Node, error) {
	child := &node{s: n.s, virtPath: path.Join(n.virtPath, req.Name)}
	if child.access() != AccessRW {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "mkdir", Path: child.absPath(), Verdict: "deny"})
		return nil, syscall.EROFS
	}
	if err := os.Mkdir(child.hostPath(), 0o755); err != nil {
		n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "mkdir", Path: child.absPath(), Verdict: "error", Err: err.Error()})
		return nil, mapErr(err)
	}
	n.s.audit(AuditEvent{At: time.Now(), Type: "filesystem", Op: "mkdir", Path: child.absPath(), Verdict: "allow"})
	return child, nil
}

// Rename moves a child of n to newDir under a new name. Both endpoints
// must have rw access — the rule trie is consulted for the source
// (preventing exfiltration of a deny-listed file via rename out of its
// directory) and for the destination (preventing a write into a
// deny-listed location). Auditing emits one event with both paths.
func (n *node) Rename(ctx context.Context, req *fuse.RenameRequest, newDir bazilfs.Node) error {
	dst, ok := newDir.(*node)
	if !ok {
		return syscall.EXDEV
	}
	oldAbs := n.childAbs(req.OldName)
	newAbs := dst.childAbs(req.NewName)
	if n.s.cfg.ACLs.Eval(oldAbs) != AccessRW || n.s.cfg.ACLs.Eval(newAbs) != AccessRW {
		n.s.audit(AuditEvent{
			At: time.Now(), Type: "filesystem", Op: "rename",
			Path: oldAbs + " → " + newAbs, Verdict: "deny",
		})
		return syscall.EROFS
	}
	oldHost := filepath.Join(n.hostPath(), req.OldName)
	newHost := filepath.Join(dst.hostPath(), req.NewName)
	if err := os.Rename(oldHost, newHost); err != nil {
		n.s.audit(AuditEvent{
			At: time.Now(), Type: "filesystem", Op: "rename",
			Path: oldAbs + " → " + newAbs, Verdict: "error", Err: err.Error(),
		})
		return mapErr(err)
	}
	n.s.audit(AuditEvent{
		At: time.Now(), Type: "filesystem", Op: "rename",
		Path: oldAbs + " → " + newAbs, Verdict: "allow",
	})
	return nil
}

// Fsync is a no-op (we write through to the host file for the prototype).
func (n *node) Fsync(ctx context.Context, req *fuse.FsyncRequest) error { return nil }

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
}
