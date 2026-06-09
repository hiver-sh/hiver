package controller

import (
	"context"
	"errors"

	gen "github.com/hiver-sh/hiver/internal/api/gen/controller"
	sandboxgen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

// ErrSandboxNotFound is returned when an operation targets a sandbox that does not exist.
var ErrSandboxNotFound = errors.New("sandbox not found")

// SandboxRuntime abstracts how sandboxes are provisioned and torn down,
// keeping the HTTP layer independent of the container platform.
type SandboxRuntime interface {
	// Lookup reports whether the sandbox for key is running and returns its
	// descriptor. Returns (false, gen.Sandbox{}, nil) when no sandbox exists.
	Lookup(key string) (running bool, sandbox gen.Sandbox, err error)

	// Start creates and starts a new sandbox for key from cfg, assigning it a
	// server-generated id and returning its descriptor.
	Start(key string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, error)

	// List returns all currently running sandboxes.
	List() ([]gen.Sandbox, error)

	// Shutdown stops and removes the sandbox for key.
	// Returns ErrSandboxNotFound if no sandbox exists for key.
	Shutdown(key string) error

	// Events streams sandbox lifecycle events until ctx is cancelled.
	// The returned channel is closed when the stream ends.
	Events(ctx context.Context) (<-chan gen.SandboxLifecycleEvent, error)
}
