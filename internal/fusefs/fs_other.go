//go:build !linux

package fusefs

import (
	"context"
	"errors"
	"io"

	"github.com/sandbox-platform/agent-sandbox/internal/remotefs"
)

// Config drives a [Server]. See fs_linux.go for the real implementation.
// On non-Linux this is a stub so dependent packages still build, but
// [Mount] returns an error.
type Config struct {
	MountPoint string
	Backend    string
	ACLs       *ACLs
	Audit      io.Writer
	Oplog      *Oplog
	Remote     remotefs.Store
}

// AuditEvent is the schema for filesystem audit events.
type AuditEvent struct{}

// Server is a stub on non-Linux platforms.
type Server struct{}

// Mount returns an error on non-Linux.
func Mount(cfg Config) (*Server, error) {
	return nil, errors.New("fusefs: only supported on Linux")
}

// Serve is a no-op stub.
func (s *Server) Serve(ctx context.Context) error {
	return errors.New("fusefs: not supported on this platform")
}

// Unmount is a no-op stub.
func (s *Server) Unmount() error { return nil }
