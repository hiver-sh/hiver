// Package spec defines the on-wire format that drives sandboxd.
//
// This is a prototype-shaped subset of the full sandbox spec described in
// DESIGN.md §5. It carries just the fields the prototype actually consumes:
// what to run as the agent, what host directory backs the workspace, the
// FUSE ACLs, the egress allowlist, and where audit logs land.
//
// Files may be YAML or JSON — sigs.k8s.io/yaml decodes via JSON internally
// so the existing `json:"…"` struct tags drive both formats. The tests
// keep the spec next to the agent fixture as `spec.yaml` for readability.
package spec

import (
	"errors"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"

	"github.com/sandbox-platform/agent-sandbox/internal/fusefs"
	"github.com/sandbox-platform/agent-sandbox/internal/proxy"
)

// Spec is the root document. Loaded by sandboxd via [Load].
type Spec struct {
	Agent    Agent  `json:"agent"`
	FS       FS     `json:"fs"`
	Egress   Egress `json:"egress"`
	AuditDir string `json:"audit_dir"`
}

// Agent describes the workload sandboxd will launch once the sidecars are up.
//
// image is the host-side directory containing the agent's Dockerfile.
// It's the orchestrator's hint for where to build the agent image from
// — sandboxd itself doesn't read it, since by the time sandboxd starts
// the orchestrator has already built + saved the image to the fixed
// in-container path [AgentImageTar]. Relative paths are resolved
// against the directory of the spec file.
//
// Env are extra KEY=VAL entries appended to the env that comes from the
// agent image's own config (image entrypoint + image env are honored).
type Agent struct {
	Image string   `json:"image,omitempty"`
	Env   []string `json:"env,omitempty"`
}

// AgentImageTar is the in-container path sandboxd reads the agent's
// docker-archive tarball from. The host-side orchestrator builds the
// image from [Agent.Path] and bind-mounts the resulting tarball here
// before launching the sandbox-pod.
const AgentImageTar = "/mnt/agent.tar"

// FS defines the per-sandbox FUSE workspace. Mount is where it appears
// to the agent; Backend names the storage flavor that backs it; ACLs
// are evaluated longest-prefix-match, default-deny (DESIGN.md §8.2);
// AuditReads turns on per-Read auditing (off by default — the kernel
// chunks user-level reads into many FUSE Reads, so this can be chatty).
//
// Initial workspace content comes from the agent image itself: any
// files in the agent rootfs at fs.mount get moved into the FUSE
// backend at sandboxd startup. Authors set this up with a normal
// COPY in the agent Dockerfile (e.g. `COPY inputs/ /workspace/inputs/`).
type FS struct {
	Mount      string        `json:"mount"`
	Backend    Backend       `json:"backend"`
	ACLs       []fusefs.Rule `json:"acls"`
	AuditReads bool          `json:"audit_reads,omitempty"`
}

// Backend names a workspace storage type. Today only "local" is
// supported (a sandboxd-managed host directory the FUSE daemon
// passthrough-mounts); future values could be "s3", "nfs", etc.
type Backend string

const (
	// BackendLocal is a sandboxd-managed directory at LocalBackendPath
	// inside the sandbox-pod. The agent never sees it directly — it
	// only sees the FUSE mount.
	BackendLocal Backend = "local"

	// LocalBackendPath is the in-pod host directory the local backend
	// uses for storage. Hardcoded since the agent doesn't get to
	// choose it (and shouldn't know it exists).
	LocalBackendPath = "/workspace-backend"
)

// HostPath resolves a Backend to the in-pod path the FUSE daemon
// overlays. Returns "" for unknown backends — Validate catches that.
func (b Backend) HostPath() string {
	switch b {
	case BackendLocal:
		return LocalBackendPath
	}
	return ""
}

// Egress controls the MITM proxy allowlist. Rules are evaluated
// top-to-bottom; the first match wins. The canonical [proxy.EgressRule]
// definition lives in the proxy package since it's the consumer.
type Egress struct {
	Allow []proxy.EgressRule `json:"allow"`
}

// Load reads and validates a spec file.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("spec: read %s: %w", path, err)
	}
	var s Spec
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("spec: parse %s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("spec: invalid %s: %w", path, err)
	}
	return &s, nil
}

// Validate enforces required-field invariants.
func (s *Spec) Validate() error {
	if s.FS.Backend == "" {
		return errors.New("fs.backend is required")
	}
	if s.FS.Backend.HostPath() == "" {
		return fmt.Errorf("fs.backend: unknown value %q (supported: %q)", s.FS.Backend, BackendLocal)
	}
	if s.FS.Mount == "" {
		return errors.New("fs.mount is required")
	}
	if s.AuditDir == "" {
		return errors.New("audit_dir is required")
	}
	return nil
}
