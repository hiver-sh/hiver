package controller

import (
	"errors"

	gen "github.com/blasten/hive/internal/api/gen/controller"
	sandboxgen "github.com/blasten/hive/internal/api/gen/sandbox"
)

// ErrSandboxNotFound is returned when an operation targets a sandbox that does not exist.
var ErrSandboxNotFound = errors.New("sandbox not found")

// SandboxRuntime abstracts how sandboxes are provisioned and torn down,
// keeping the HTTP layer independent of the container platform.
type SandboxRuntime interface {
	// Lookup reports whether the sandbox for id is running and returns its
	// descriptor. Returns (false, gen.Sandbox{}, nil) when no sandbox exists.
	Lookup(id string) (running bool, sandbox gen.Sandbox, err error)

	// Start creates and starts a new sandbox from cfg, returning its descriptor.
	Start(id string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, error)

	// List returns all currently running sandboxes.
	List() ([]gen.Sandbox, error)

	// Shutdown stops and removes the sandbox for id.
	// Returns ErrSandboxNotFound if no sandbox exists for id.
	Shutdown(id string) error
}
