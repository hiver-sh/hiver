// Package remotefs defines the pluggable storage interface that backs
// non-local FUSE workspaces. fusefs serves the agent's hot path from a
// local buffer; mutating operations (create / write / rename / remove)
// enqueue journal entries that an uploader goroutine drains into a
// [Store]. Reads only hit the buffer — the [Store] is consulted at
// mount-time bootstrap and again when a path is missing locally.
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
)

// Store is the operations a remote-backed workspace must support.
type Store interface {
	// List returns object paths under prefix. An empty prefix returns
	// every object. Implementations may paginate internally; this call
	// blocks until the full list is built.
	List(ctx context.Context, prefix string) ([]string, error)

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
