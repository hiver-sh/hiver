// Package e2e runs the full sandbox-pod prototype end-to-end on the host,
// not inside the container. The test:
//
//  1. Builds the language-agnostic sandbox-runtime image (root Dockerfile)
//     — sandboxd + sbxproxy + sbxfuse + runc, no language runtime.
//  2. Builds the agent-python image (test/e2e/fixtures/agent-python) — a
//     SEPARATE image containing python3 + agent.py with the script as
//     ENTRYPOINT.
//  3. `docker save`s the agent image into a tarball that gets bind-mounted
//     into the sandbox-pod container. sandboxd unpacks it into an OCI
//     rootfs and runs it under runc, sharing the sandbox-pod's netns and
//     bind-mounting the FUSE /workspace.
//  4. Starts two HTTP upstream servers on the host — one allowlisted, one
//     not — reachable from inside the sandbox-pod via host-gateway.
//  5. Loads test/e2e/fixtures/agent-python/spec.yaml, substitutes the
//     runtime-only fields (URLs from steps 4), and bind-mounts the result
//     at /mnt/spec.yaml. ALLOW_URL/DENY_URL/DENY_PATH ride into the
//     agent through agent.env (the spec's runc-aware forwarding hook).
//  6. Runs the sandbox-pod container, captures stdout/stderr, asserts on
//     (a) agent script output, (b) proxy audit log, (c) FUSE audit log,
//     (d) sandboxd's own lifecycle log lines.
//
// Skips automatically when Docker isn't available — runs anywhere a Docker
// daemon is reachable (Linux directly, macOS via Docker Desktop / OrbStack).
package e2e_test

import (
	"testing"
)

func TestPythonSandboxE2E(t *testing.T) {
	runFixtureE2E(t, "agent-python")
}
