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
	"encoding/json"
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
// to the agent; Backend is the discriminator that picks the storage
// flavor; ACLs are evaluated longest-prefix-match, default-deny
// (DESIGN.md §8.2); AuditReads turns on per-Read auditing (off by
// default — the kernel chunks user-level reads into many FUSE Reads,
// so this can be chatty).
//
// Initial workspace content comes from the agent image itself: any
// files in the agent rootfs at fs.mount get moved into the FUSE
// backend at sandboxd startup. Authors set this up with a normal
// COPY in the agent Dockerfile (e.g. `COPY inputs/ /workspace/inputs/`).
//
// Per-backend extras (auth tokens, folder IDs, bucket names, …) live
// inline as optional fields. Each backend reads only the fields it
// recognizes; [Spec.Validate] checks that the combination present
// matches the chosen backend.
type FS struct {
	Mount      string        `json:"mount"`
	Backend    Backend       `json:"backend"`
	ACLs       []fusefs.Rule `json:"acls"`
	AuditReads bool          `json:"audit_reads,omitempty"`

	// gdrive only — see the GoogleDrive comment block below.
	AccessToken        string `json:"access_token,omitempty"`
	RefreshToken       string `json:"refresh_token,omitempty"`
	ClientID           string `json:"client_id,omitempty"`
	ClientSecret       string `json:"client_secret,omitempty"`
	ServiceAccountJSON string `json:"service_account_json,omitempty"`
	FolderID           string `json:"folder_id,omitempty"`
}

// Backend names a workspace storage type. New backends extend this
// enum and the matching switch in [Backend.HostPath] / [Backend.IsRemote]
// + the dispatch in cmd/sbxfuse/main.go.
//
// "local" is a sandboxd-managed directory the FUSE daemon
// passthrough-mounts. Reads and writes stay local — there's no
// uploader, no oplog, no remote consistency to worry about.
//
// "gdrive" backs the FUSE mount with a write-through cache: the local
// buffer at [LocalBackendPath] serves the agent's hot path, every
// mutation enqueues an oplog entry, and an uploader goroutine drains
// it into Google Drive. The same shape applies to the planned
// "onedrive" / "s3" / "gcs" backends — they all share the
// [internal/remotefs].Store interface and only differ in the network
// client behind it.
//
// Auth tokens for "gdrive" live inline on [FS] (access_token,
// refresh_token, client_id, client_secret, service_account_json) —
// at least one of access_token or service_account_json is required.
// folder_id, when set, scopes the workspace to that Drive folder.
type Backend string

const (
	BackendLocal       Backend = "local"
	BackendGoogleDrive Backend = "gdrive"

	// LocalBackendPath is the in-pod host directory every backend uses
	// for the local buffer. For "local" it's the source of truth; for
	// the journaled backends it's a write-through cache.
	LocalBackendPath = "/workspace-backend"
)

// HostPath returns the in-pod path the FUSE daemon overlays. All
// backends today share LocalBackendPath — the difference is whether
// an oplog + remote uploader runs alongside.
func (b Backend) HostPath() string {
	switch b {
	case BackendLocal, BackendGoogleDrive:
		return LocalBackendPath
	}
	return ""
}

// IsRemote reports whether the backend writes through to a remote
// store via the oplog. Local is the only non-remote backend today.
func (b Backend) IsRemote() bool {
	switch b {
	case BackendGoogleDrive:
		return true
	}
	return false
}

// BackendConfigJSON returns the per-backend config sandboxd should hand
// to sbxfuse via -remote-config. Returns (nil, nil) for backends that
// take no config (local). The schema mirrors the matching
// [internal/remotefs] config struct so sbxfuse can json.Unmarshal
// directly without translation.
func (f *FS) BackendConfigJSON() ([]byte, error) {
	switch f.Backend {
	case BackendGoogleDrive:
		return json.Marshal(struct {
			AccessToken        string `json:"access_token,omitempty"`
			RefreshToken       string `json:"refresh_token,omitempty"`
			ClientID           string `json:"client_id,omitempty"`
			ClientSecret       string `json:"client_secret,omitempty"`
			ServiceAccountJSON string `json:"service_account_json,omitempty"`
			FolderID           string `json:"folder_id,omitempty"`
		}{
			AccessToken:        f.AccessToken,
			RefreshToken:       f.RefreshToken,
			ClientID:           f.ClientID,
			ClientSecret:       f.ClientSecret,
			ServiceAccountJSON: f.ServiceAccountJSON,
			FolderID:           f.FolderID,
		})
	}
	return nil, nil
}

// Egress controls the MITM proxy allowlist. Rules are evaluated
// top-to-bottom; the first match wins. The canonical [proxy.EgressRule]
// definition lives in the proxy package since it's the consumer.
type Egress struct {
	Allow []proxy.EgressRule `json:"allow"`
}

// Load reads and validates a spec file.
func Load(path string) (*Spec, error) {
	s, err := Parse(path)
	if err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("spec: invalid %s: %w", path, err)
	}
	return s, nil
}

// Parse reads a spec file without validating it. Useful for tests that
// load a fixture, fill in fields supplied at runtime (auth tokens
// from env vars, ports from free-port lookup, …), then validate the
// fully-formed spec themselves before handing it to sandboxd.
func Parse(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("spec: read %s: %w", path, err)
	}
	var s Spec
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("spec: parse %s: %w", path, err)
	}
	return &s, nil
}

// Validate enforces required-field invariants.
func (s *Spec) Validate() error {
	if s.FS.Backend == "" {
		return errors.New("fs.backend is required")
	}
	if s.FS.Backend.HostPath() == "" {
		return fmt.Errorf("fs.backend: unknown value %q (supported: %q, %q)", s.FS.Backend, BackendLocal, BackendGoogleDrive)
	}
	if s.FS.Backend == BackendGoogleDrive && s.FS.AccessToken == "" && s.FS.ServiceAccountJSON == "" {
		return errors.New("fs.backend gdrive: one of access_token / service_account_json is required")
	}
	if s.FS.Mount == "" {
		return errors.New("fs.mount is required")
	}
	if s.AuditDir == "" {
		return errors.New("audit_dir is required")
	}
	return nil
}
