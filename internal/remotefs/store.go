// Package remotefs defines the pluggable storage interface that backs
// non-local FUSE workspaces. fusefs treats the local backend dir as a
// **write buffer only**: mutating operations land there first, an
// uploader goroutine drains them into a [Store], and the local copy
// is evicted once the upload acks. Read operations (Lookup, Attr,
// ReadDirAll, Open) consult the [Store] directly so the agent always
// sees the current upstream state.
//
// The interface is path-keyed because every target we plan to support
// flattens to "string identifier → bytes":
//
//   - S3 / GCS: native key-value, slashes in keys are convention only.
//   - Google Drive / OneDrive: ID-based, but every implementation is
//     expected to maintain a path → ID map internally so callers
//     never see Drive IDs.
//
// All paths are forward-slash, rooted at /. Implementations must treat
// them as opaque keys; folder structure is the orchestrator's concern.
package remotefs

import (
	"context"
	"errors"
	"io"
	"time"
)

// FileInfo is the metadata Stat returns. Mirrors the subset of
// [os.FileInfo] fusefs actually needs for FUSE Attr / Lookup.
type FileInfo struct {
	Path  string    // canonical, forward-slash, rooted at /
	Size  int64     // byte length; 0 for directories
	Mtime time.Time // last modified; zero if unknown
	IsDir bool
	// Symlink marks the entry as a symbolic link. How that bit is stored
	// upstream is each backend's business — a native FS symlink, blob
	// custom-metadata, a Drive appProperty, or a sidecar object — but the
	// FUSE layer only ever sees this flag. When true, IsDir is false and
	// LinkTarget holds the link's target.
	Symlink    bool
	LinkTarget string // the target path, valid only when Symlink is true
}

// Store is the operations a remote-backed workspace must support.
type Store interface {
	// List returns object paths under prefix. An empty prefix returns
	// every object. Implementations may paginate internally; this call
	// blocks until the full list is built.
	List(ctx context.Context, prefix string) ([]string, error)

	// ListDir returns the immediate children of dir (one level deep,
	// unlike List which recurses). Each entry's Path is its full
	// agent-visible path; IsDir distinguishes folders from files.
	// Returns [ErrNotExist] if dir itself doesn't exist on the remote.
	// Used by fusefs ReadDirAll so a directory listing is one API call,
	// not a recursive tree walk.
	ListDir(ctx context.Context, dir string) ([]FileInfo, error)

	// Stat returns metadata for the object at path. Returns
	// [ErrNotExist] for missing objects. Used by fusefs Lookup/Attr
	// so we can answer "does this exist? how big?" without downloading
	// the body. A symlink is reported with FileInfo.Symlink=true and its
	// target in FileInfo.LinkTarget — in one call, with no second probe.
	Stat(ctx context.Context, path string) (FileInfo, error)

	// Symlink persists a symbolic link at path pointing at target,
	// creating or replacing whatever was there. Backends store the link
	// however suits them (a native FS symlink, blob custom-metadata, a
	// Drive appProperty, or a sidecar object); the only contract is that a
	// subsequent Stat/ListDir reports it with FileInfo.Symlink=true and
	// LinkTarget=target. Replaces the old convention of writing a separate
	// "<path>.symlink" object, so a symlink no longer costs the FUSE layer
	// a second key probe on every miss.
	Symlink(ctx context.Context, path, target string) error

	// Readlink returns the target of the symlink at path. Backends that
	// carry the target in metadata (GCS, Azure, Drive) or natively (local
	// FS) answer from the same place Stat reads; sidecar-object backends
	// (S3, OneDrive) read the object body. Returns [ErrNotExist] when path
	// is not a symlink. Called by fusefs Readlink for a link that isn't in
	// the local buffer.
	Readlink(ctx context.Context, path string) (string, error)

	// Get returns the content of the object at path. Caller must Close.
	// Returns [ErrNotExist] for missing objects (do not synthesize zero
	// bytes — fusefs uses a "not on remote" signal to distinguish
	// "exists, empty" from "doesn't exist yet").
	Get(ctx context.Context, path string) (io.ReadCloser, error)

	// Put writes content to path, creating or replacing. content is
	// streamed; implementations should not buffer the entire body in
	// memory before sending.
	Put(ctx context.Context, path string, content io.Reader) error

	// Delete removes the object at path. Idempotent — a missing object
	// is not an error.
	Delete(ctx context.Context, path string) error

	// Move renames src to dst. Implementations without a native rename
	// (S3, GCS) compose Get + Put + Delete; ones with one (Drive,
	// OneDrive) call the underlying API directly.
	Move(ctx context.Context, src, dst string) error
}

// ErrNotExist signals that a Get target doesn't exist on the remote.
var ErrNotExist = errors.New("remote: object does not exist")

// symlinkSuffix is the sidecar-object representation used only by backends
// whose listing API can't return custom metadata (S3, OneDrive): a symlink
// at <path> is stored as a regular object at <path>+symlinkSuffix whose body
// is the target. It is an internal implementation detail of those backends —
// fusefs and every metadata-native backend (GCS, Azure, Drive, local) never
// use it, and it never leaks past Stat/ListDir, which fold it back into a
// FileInfo.Symlink under the plain name.
const symlinkSuffix = ".symlink"

// symlinkMetaKey is the custom-metadata key backends set to mark an object a
// symlink; symlinkTargetMetaKey holds the target. Blob stores (GCS, Azure)
// and Drive (as an appProperty) carry these on the object itself, so one
// Stat/ListDir returns the bit and the target with no extra round-trip.
const (
	symlinkMetaKey       = "hiver_symlink"
	symlinkTargetMetaKey = "hiver_symlink_target"
)
