package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	gen "github.com/hiver-sh/hiver/internal/api/gen/controller"
	sandboxgen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
)

// ErrSandboxNotFound is returned when an operation targets a sandbox that does not exist.
var ErrSandboxNotFound = errors.New("sandbox not found")

// usesSnapshotMount reports whether the config routes snapshots to a FUSE drive
// (snapshot.mount) rather than the runtime's local snapshot directory. When it
// does, the runtime skips provisioning the local /snapshots volume entirely:
// the volume would be unnecessary and, if the FUSE drive is mounted at the same
// path, would be shadowed by it.
func usesSnapshotMount(cfg sandboxgen.SandboxConfig) bool {
	return cfg.Snapshot != nil && cfg.Snapshot.Mount != nil && *cfg.Snapshot.Mount != ""
}

const (
	readyProbeInterval  = 250 * time.Millisecond
	sandboxReadyTimeout = 120 * time.Second
)

// waitSandboxReady blocks until the sandboxd at host:sandboxdPort reports ready
// or ctx/timeout expires. It long-polls /v1/ping?block=true: sandboxd serves
// that endpoint before the workload is up (returning 503 until NotifyReady
// fires), and ?block=true makes it wait for readiness and return 200 the moment
// it flips — so once the port is listening a single request usually suffices.
// The connect is retried because the sandbox has just started and sandboxd may
// not have bound :sandboxdPort yet. host is the container's id-derived network
// alias under docker (shared hiver_default network) or the pod IP under k8s,
// both reachable from the controller. Shared by both SandboxRuntime backends.
func waitSandboxReady(ctx context.Context, host string) error {
	ctx, cancel := context.WithTimeout(ctx, sandboxReadyTimeout)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/v1/ping?block=true", host, sandboxdPort)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := sandboxHTTPClient.Do(req)
		if err == nil {
			status := resp.StatusCode
			drainAndClose(resp)
			if status == http.StatusOK {
				return nil
			}
		}
		// Connection refused (sandboxd not listening yet) or a non-200 (its
		// long-poll was cut short): back off and retry while time remains.
		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox did not become ready within %s", sandboxReadyTimeout)
		case <-time.After(readyProbeInterval):
		}
	}
}

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

	// Shutdown stops and removes the sandbox for key.
	// Returns ErrSandboxNotFound if no sandbox exists for key.
	Shutdown(ctx context.Context, key string) error

	// Events streams sandbox lifecycle events until ctx is cancelled.
	// The returned channel is closed when the stream ends.
	Events(ctx context.Context) (<-chan gen.SandboxLifecycleEvent, error)
}
