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
	Agent     Agent     `json:"agent"`
	Workspace Workspace `json:"workspace"`
	Egress    Egress    `json:"egress"`
	AuditDir  string    `json:"audit_dir"`
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

// Workspace defines the per-sandbox FUSE mount and its ACLs.
//
// Backend is the host directory the FUSE daemon overlays.
// Mount is where it appears to the agent.
// ACLs are evaluated longest-prefix-match, default-deny (DESIGN.md §8.2).
//
// AuditReads turns on per-Read auditing. Off by default — the kernel
// chunks user-level reads into many FUSE Read calls, so this can be very
// chatty. Open is always audited regardless.
type Workspace struct {
	Backend    string        `json:"backend"`
	Mount      string        `json:"mount"`
	ACLs       []fusefs.Rule `json:"acls"`
	AuditReads bool          `json:"audit_reads,omitempty"`
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
	if s.Workspace.Backend == "" {
		return errors.New("workspace.backend is required")
	}
	if s.Workspace.Mount == "" {
		return errors.New("workspace.mount is required")
	}
	if s.AuditDir == "" {
		return errors.New("audit_dir is required")
	}
	return nil
}
