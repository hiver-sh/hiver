package controller

import (
	"context"
	"time"

	gen "github.com/hiver-sh/hiver/internal/api/gen/controller"
	sandboxgen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

const (
	// readyProbeInterval is the backoff between create-POST retries while a
	// just-started pod's sandboxd is not yet accepting requests.
	readyProbeInterval = 250 * time.Millisecond
	// sandboxReadyTimeout bounds how long a create waits for a just-started pod's
	// sandboxd to accept the POST /v1/<key> that brings the sandbox up.
	sandboxReadyTimeout = 120 * time.Second
)

// SandboxRuntime abstracts how sandboxes are provisioned and torn down,
// keeping the HTTP layer independent of the container platform.
type SandboxRuntime interface {
	// Lookup reports whether the sandbox for key is running and returns its
	// descriptor. Returns (false, gen.Sandbox{}, nil) when no sandbox exists.
	// ctx bounds any wait for the sandbox to become reachable.
	Lookup(ctx context.Context, key string) (running bool, sandbox gen.Sandbox, err error)

	// Start creates and starts a new sandbox for key from cfg, assigning it a
	// server-generated id and returning its descriptor. ctx bounds the wait for
	// the sandbox to become reachable; the durable key reservation is created
	// regardless so it survives ctx cancellation.
	Start(ctx context.Context, key string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, error)

	// List returns all currently running sandboxes.
	List(ctx context.Context) ([]gen.Sandbox, error)

	// Events streams sandbox lifecycle events until ctx is cancelled.
	// The returned channel is closed when the stream ends.
	Events(ctx context.Context) (<-chan gen.SandboxLifecycleEvent, error)
}
