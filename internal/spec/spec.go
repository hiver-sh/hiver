// Package spec defines the on-wire format that drives sandboxd.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/hiver-sh/hiver/internal/fusefs"
	"github.com/hiver-sh/hiver/internal/proxy"
)

var snapshotKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// BackendSuffix is appended to a mount path to derive the host-side
// backend directory (e.g. "/workspace" → "/workspace-backend").
const BackendSuffix = "-backend"

// Spec is the root document. Loaded by sandboxd via [Load].
type Spec struct {
	Image      string             `json:"image,omitempty"`
	CPU        *float64           `json:"cpu,omitempty"`
	RequestCPU *float64           `json:"request_cpu,omitempty"`
	Memory     *int               `json:"memory,omitempty"`
	Entrypoint Entrypoint         `json:"entrypoint,omitempty"`
	Cwd        string             `json:"cwd,omitempty"`
	Tty        *bool              `json:"tty,omitempty"`
	Ttl        *int               `json:"ttl,omitempty"`
	Env        map[string]string  `json:"env,omitempty"`
	FS         []FS               `json:"fs"`
	Egress     []proxy.EgressRule `json:"egress,omitempty"`
	Snapshot   *Snapshot          `json:"snapshot,omitempty"`
}

// Entrypoint is an argv override for the workload. On the wire it accepts
// either a JSON array of strings (each element a separate argument) or a
// single JSON string, which is split on whitespace into arguments. Both forms
// normalize to the same []string, so the rest of sandboxd treats it uniformly.
type Entrypoint []string

// UnmarshalJSON accepts either a string ("tail -f /dev/null") or an array of
// strings (["tail", "-f", "/dev/null"]) and normalizes to argv form.
func (e *Entrypoint) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*e = nil
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("entrypoint: %w", err)
		}
		*e = strings.Fields(s)
		return nil
	}
	var argv []string
	if err := json.Unmarshal(data, &argv); err != nil {
		return fmt.Errorf("entrypoint: %w", err)
	}
	*e = argv
	return nil
}

// Snapshot controls how the sandbox upper layer is persisted and restored.
type Snapshot struct {
	// RestoreKey identifies which snapshot file to restore on start.
	// When empty, no restore is performed.
	RestoreKey string `json:"restore_key,omitempty"`
	// WriteKey is the key used when saving the snapshot on shutdown.
	// When empty, RestoreKey is used as the write key.
	WriteKey string `json:"write_key,omitempty"`
	// Include is a list of absolute container paths or glob patterns
	// (e.g. /home/user/*) whose parent directories are snapshotted.
	Include []string `json:"include,omitempty"`
	// Mount is the mount path of an FS entry where snapshot tarballs are
	// written and read, instead of the host's local snapshot directory.
	// Point it at an internal, remote-backed FS to persist and restore
	// snapshots through a FUSE drive. When empty, the host's local snapshot
	// directory is used.
	Mount string `json:"mount,omitempty"`
}

// EffectiveWriteKey returns the key to use when saving the snapshot.
func (s *Snapshot) EffectiveWriteKey() string {
	if s.WriteKey != "" {
		return s.WriteKey
	}
	return s.RestoreKey
}

// FS defines one FUSE workspace. A spec carries a list of these so
// agents can mount multiple workspaces side-by-side (e.g. a local
// scratch dir plus a write-through gdrive mount). Mount is where it
// appears to the agent; Backend is the discriminator that picks the
// storage flavor; ACLs are evaluated longest-prefix-match, default-deny
// (DESIGN.md §8.2);
//
// Initial workspace content comes from the agent image itself: any
// files in the agent rootfs at the mount path get moved into the FUSE
// backend at sandboxd startup. Authors set this up with a normal
// COPY in the agent Dockerfile (e.g. `COPY inputs/ /workspace/inputs/`).
//
// Per-backend extras (auth tokens, folder IDs, bucket names, …) live
// inline as optional fields. Each backend reads only the fields it
// recognizes; [Spec.Validate] checks that the combination present
// matches the chosen backend.
type FS struct {
	Mount   string        `json:"mount"`
	Backend Backend       `json:"backend"`
	ACLs    []fusefs.Rule `json:"acls"`

	// Internal, when true, mounts the FUSE workspace on the sandbox host but
	// does not export it into the agent workload — the agent never sees Mount.
	// Used for storage the sandbox needs but the agent must not access, e.g. a
	// remote-backed snapshot target referenced by Snapshot.Mount.
	Internal bool `json:"internal,omitempty"`

	// Per-backend extras live inline with a backend-name prefix so it's
	// obvious from the YAML which backend a field belongs to. Only the
	// fields matching `backend` are read; the rest are ignored.

	// gdrive only — auth tokens and target folder.
	GdriveAccessToken        string `json:"gdrive_access_token,omitempty"`
	GdriveRefreshToken       string `json:"gdrive_refresh_token,omitempty"`
	GdriveClientID           string `json:"gdrive_client_id,omitempty"`
	GdriveClientSecret       string `json:"gdrive_client_secret,omitempty"`
	GdriveServiceAccountJSON string `json:"gdrive_service_account_json,omitempty"`
	GdriveFolderID           string `json:"gdrive_folder_id,omitempty"`
	GdrivePrefix             string `json:"gdrive_prefix,omitempty"`

	// gcs only — bucket, optional key prefix, and optional service account.
	GcsBucket             string `json:"gcs_bucket,omitempty"`
	GcsPrefix             string `json:"gcs_prefix,omitempty"`
	GcsServiceAccountJSON string `json:"gcs_service_account_json,omitempty"`

	// external only — base URL of the HTTP host backing the file system.
	Host string `json:"host,omitempty"`
}

// Backend names a workspace storage type. New backends extend this
// enum and the matching switch in [Backend.Valid] / [Backend.IsRemote]
// + the dispatch in cmd/sbxfuse/main.go.
//
// "local" is a sandboxd-managed directory the FUSE daemon
// passthrough-mounts. Reads and writes stay local — there's no
// uploader, no oplog, no remote consistency to worry about.
//
// "gdrive" backs the FUSE mount with a write-through cache: a local
// buffer derived from the mount path ([FS.BackendPath]) serves the
// agent's hot path, every mutation enqueues an oplog entry, and an
// uploader goroutine drains it into Google Drive. The same shape
// applies to the planned "onedrive" / "s3" / "gcs" backends — they
// all share the [internal/remotefs].Store interface and only differ
// in the network client behind it.
//
// Auth tokens for "gdrive" live inline on [FS] (access_token,
// refresh_token, client_id, client_secret, service_account_json) —
// at least one of access_token or service_account_json is required.
// folder_id, when set, scopes the workspace to that Drive folder.
type Backend string

const (
	BackendLocal              Backend = "local"
	BackendGoogleDrive        Backend = "gdrive"
	BackendGoogleCloudStorage Backend = "gcs"
	BackendExternal           Backend = "external"
)

// Valid reports whether the backend is one sandboxd knows how to wire up.
func (b Backend) Valid() bool {
	switch b {
	case BackendLocal, BackendGoogleDrive, BackendGoogleCloudStorage, BackendExternal:
		return true
	}
	return false
}

// IsRemote reports whether the backend writes through to a remote
// store via the oplog. Local is the only non-remote backend today.
func (b Backend) IsRemote() bool {
	switch b {
	case BackendGoogleDrive, BackendGoogleCloudStorage, BackendExternal:
		return true
	}
	return false
}

// BackendPath returns the in-pod host directory that backs this mount —
// the local buffer for remote backends, the source of truth for local.
// Derived from the mount so each FS entry gets its own dir without the
// caller having to thread per-mount config through.
func (f *FS) BackendPath() string {
	return f.Mount + BackendSuffix
}

// Slug returns a filename-safe identifier derived from the mount path,
// used by sandboxd to name per-mount sidecar files (ACL JSON, audit log).
func (f *FS) Slug() string {
	s := strings.Trim(f.Mount, "/")
	s = strings.ReplaceAll(s, "/", "-")
	if s == "" {
		return "root"
	}
	return s
}

// Env-var fallbacks for gdrive credentials. Spec fields take precedence;
// the env vars fill in only when the matching spec field is empty.
const (
	envGdriveAccessToken        = "HIVE_GDRIVE_ACCESS_TOKEN"
	envGdriveRefreshToken       = "HIVE_GDRIVE_REFRESH_TOKEN"
	envGdriveClientID           = "HIVE_GDRIVE_CLIENT_ID"
	envGdriveClientSecret       = "HIVE_GDRIVE_CLIENT_SECRET"
	envGdriveServiceAccountJSON = "HIVE_GDRIVE_SERVICE_ACCOUNT_JSON"
	envGdriveFolderID           = "HIVE_GDRIVE_FOLDER_ID"
	envGdrivePrefix             = "HIVE_GDRIVE_PREFIX"
)

// Env-var fallbacks for gcs credentials.
const (
	envGcsBucket             = "HIVE_GCS_BUCKET"
	envGcsPrefix             = "HIVE_GCS_PREFIX"
	envGcsServiceAccountJSON = "HIVE_GCS_SERVICE_ACCOUNT_JSON"
)

// Env-var fallback for the external backend host.
const envExternalHost = "HIVE_EXTERNAL_HOST"

func or(value, envKey string) string {
	if value != "" {
		return value
	}
	return os.Getenv(envKey)
}

// gdriveResolved returns the effective gdrive credentials — spec
// fields with env-var fallback. Used by both Validate (to check that
// at least one credential is present) and BackendConfigJSON (to build
// the JSON sbxfuse receives).
func (f *FS) gdriveResolved() (accessToken, refreshToken, clientID, clientSecret, serviceAccountJSON, folderID, prefix string) {
	return or(f.GdriveAccessToken, envGdriveAccessToken),
		or(f.GdriveRefreshToken, envGdriveRefreshToken),
		or(f.GdriveClientID, envGdriveClientID),
		or(f.GdriveClientSecret, envGdriveClientSecret),
		or(f.GdriveServiceAccountJSON, envGdriveServiceAccountJSON),
		or(f.GdriveFolderID, envGdriveFolderID),
		or(f.GdrivePrefix, envGdrivePrefix)
}

// gcsResolved returns the effective gcs config — spec fields with env-var fallback.
func (f *FS) gcsResolved() (bucket, prefix, serviceAccountJSON string) {
	return or(f.GcsBucket, envGcsBucket),
		or(f.GcsPrefix, envGcsPrefix),
		or(f.GcsServiceAccountJSON, envGcsServiceAccountJSON)
}

// externalResolved returns the effective external host — spec field with
// env-var fallback.
func (f *FS) externalResolved() (host string) {
	return or(f.Host, envExternalHost)
}

// BackendConfigJSON returns the per-backend config sandboxd should hand
// to sbxfuse via -remote-config. Returns (nil, nil) for backends that
// take no config (local). The schema mirrors the matching
// [internal/remotefs] config struct so sbxfuse can json.Unmarshal
// directly without translation.
func (f *FS) BackendConfigJSON() ([]byte, error) {
	switch f.Backend {
	case BackendGoogleDrive:
		access, refresh, clientID, clientSecret, sa, folder, prefix := f.gdriveResolved()
		return json.Marshal(struct {
			AccessToken        string `json:"access_token,omitempty"`
			RefreshToken       string `json:"refresh_token,omitempty"`
			ClientID           string `json:"client_id,omitempty"`
			ClientSecret       string `json:"client_secret,omitempty"`
			ServiceAccountJSON string `json:"service_account_json,omitempty"`
			FolderID           string `json:"folder_id,omitempty"`
			Prefix             string `json:"prefix,omitempty"`
		}{
			AccessToken:        access,
			RefreshToken:       refresh,
			ClientID:           clientID,
			ClientSecret:       clientSecret,
			ServiceAccountJSON: sa,
			FolderID:           folder,
			Prefix:             prefix,
		})
	case BackendGoogleCloudStorage:
		bucket, prefix, sa := f.gcsResolved()
		return json.Marshal(struct {
			Bucket             string `json:"bucket"`
			Prefix             string `json:"prefix,omitempty"`
			ServiceAccountJSON string `json:"service_account_json,omitempty"`
		}{
			Bucket:             bucket,
			Prefix:             prefix,
			ServiceAccountJSON: sa,
		})
	case BackendExternal:
		return json.Marshal(struct {
			Host string `json:"host"`
		}{
			Host: f.externalResolved(),
		})
	}
	return nil, nil
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

// EnvSpec is the environment variable sandboxd reads the spec JSON from.
// The runtimes (docker, k8s) inject the marshalled SandboxConfig here
// instead of mounting it as a file.
const EnvSpec = "HIVE_SPEC"

// LoadEnv reads and validates the spec from the EnvSpec environment variable.
func LoadEnv() (*Spec, error) {
	data := os.Getenv(EnvSpec)
	if data == "" {
		return nil, fmt.Errorf("spec: %s is empty", EnvSpec)
	}
	var s Spec
	if err := json.Unmarshal([]byte(data), &s); err != nil {
		return nil, fmt.Errorf("spec: parse %s: %w", EnvSpec, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("spec: invalid %s: %w", EnvSpec, err)
	}
	return &s, nil
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
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("spec: parse %s: %w", path, err)
	}
	return &s, nil
}

// Validate enforces required-field invariants.
func (s *Spec) Validate() error {
	if s.CPU != nil && *s.CPU <= 0 {
		return fmt.Errorf("cpu: must be > 0, got %g", *s.CPU)
	}
	if s.RequestCPU != nil && *s.RequestCPU <= 0 {
		return fmt.Errorf("request_cpu: must be > 0, got %g", *s.RequestCPU)
	}
	if s.Memory != nil && *s.Memory < 128 {
		return fmt.Errorf("memory: must be >= 128 (MiB), got %d", *s.Memory)
	}
	// fs is optional: a prewarm sandbox boots with only an image and no
	// mounts, then receives its real filesystem via the first PUT /v1/config.
	// Any entries that are present must still be well-formed.
	for i := range s.FS {
		f := &s.FS[i]
		ctx := fmt.Sprintf("fs[%d]", i)
		if f.Backend == "" {
			return fmt.Errorf("%s.backend is required", ctx)
		}
		if !f.Backend.Valid() {
			return fmt.Errorf("%s.backend: unknown value %q (supported: %q, %q, %q, %q)", ctx, f.Backend, BackendLocal, BackendGoogleDrive, BackendGoogleCloudStorage, BackendExternal)
		}
		if f.Backend == BackendGoogleDrive {
			access, _, _, _, sa, _, _ := f.gdriveResolved()
			if access == "" && sa == "" {
				return fmt.Errorf("%s.backend gdrive: one of gdrive_access_token / gdrive_service_account_json is required (or env %s / %s)", ctx, envGdriveAccessToken, envGdriveServiceAccountJSON)
			}
		}
		if f.Backend == BackendGoogleCloudStorage {
			bucket, _, sa := f.gcsResolved()
			if bucket == "" {
				return fmt.Errorf("%s.backend gcs: gcs_bucket is required (or env %s)", ctx, envGcsBucket)
			}
			if sa == "" {
				return fmt.Errorf("%s.backend gcs: gcs_service_account_json is required (or env %s)", ctx, envGcsServiceAccountJSON)
			}
		}
		if f.Backend == BackendExternal {
			if f.externalResolved() == "" {
				return fmt.Errorf("%s.backend external: host is required (or env %s)", ctx, envExternalHost)
			}
		}
		if f.Mount == "" {
			return fmt.Errorf("%s.mount is required", ctx)
		}
		if !strings.HasPrefix(f.Mount, "/") {
			return fmt.Errorf("%s.mount: must be an absolute path, got %q", ctx, f.Mount)
		}
		if len(f.ACLs) == 0 {
			f.ACLs = []fusefs.Rule{{Path: f.Mount + "/**", Access: fusefs.AccessRW}}
		}
	}
	// Mount paths must be unique and non-overlapping: one being a
	// prefix of another would let bind-mounts and ACLs collide. Compare
	// every pair as path strings, treating "/a" as a prefix of "/a/b"
	// but not of "/ab".
	for i := range s.FS {
		for j := i + 1; j < len(s.FS); j++ {
			a, b := s.FS[i].Mount, s.FS[j].Mount
			if pathOverlaps(a, b) {
				return fmt.Errorf("fs[%d].mount %q overlaps fs[%d].mount %q", i, a, j, b)
			}
		}
	}
	for i, e := range s.Egress {
		ctx := fmt.Sprintf("egress[%d]", i)
		if strings.TrimSpace(e.Host) == "" {
			return fmt.Errorf("%s.host is required", ctx)
		}
		for j, p := range e.Ports {
			if p < 1 || p > 65535 {
				return fmt.Errorf("%s.ports[%d]: %d out of range [1, 65535]", ctx, j, p)
			}
		}
		if ov := e.Override; ov != nil && ov.Host != "" {
			if err := validateOverrideHost(ov.Host, e.Access); err != nil {
				return fmt.Errorf("%s.override.host: %w", ctx, err)
			}
		}
		if ov := e.Override; ov != nil && ov.PrefixPath != "" {
			if err := validateOverridePrefixPath(ov.PrefixPath, e.Access); err != nil {
				return fmt.Errorf("%s.override.prefix_path: %w", ctx, err)
			}
		}
	}
	if sn := s.Snapshot; sn != nil {
		if sn.RestoreKey != "" && !snapshotKeyRE.MatchString(sn.RestoreKey) {
			return fmt.Errorf("snapshot.restore_key: must match %s", snapshotKeyRE)
		}
		if sn.WriteKey != "" && !snapshotKeyRE.MatchString(sn.WriteKey) {
			return fmt.Errorf("snapshot.write_key: must match %s", snapshotKeyRE)
		}
		if sn.Mount != "" {
			if !strings.HasPrefix(sn.Mount, "/") {
				return fmt.Errorf("snapshot.mount: must be an absolute path, got %q", sn.Mount)
			}
			// Must name a declared FS mount so the tarball lands on a real
			// FUSE drive rather than a stray host directory.
			found := false
			for i := range s.FS {
				if s.FS[i].Mount == sn.Mount {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("snapshot.mount %q does not match any fs[].mount", sn.Mount)
			}
		}
	}
	return nil
}

// validateOverrideHost checks an EgressRule override.host value:
// `hostname[:port]` or `ip[:port]` — no scheme, no path, no wildcard —
// and only meaningful on allow rules (a denied request is never dialed).
func validateOverrideHost(v, access string) error {
	if access != "allow" {
		return errors.New("only valid on allow rules")
	}
	if strings.Contains(v, "://") || strings.Contains(v, "/") {
		return fmt.Errorf("must be host[:port] without scheme or path, got %q", v)
	}
	if strings.Contains(v, "*") {
		return fmt.Errorf("wildcards are not allowed, got %q", v)
	}
	if host, portStr, err := net.SplitHostPort(v); err == nil {
		if strings.TrimSpace(host) == "" {
			return fmt.Errorf("missing hostname in %q", v)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("port %q out of range [1, 65535]", portStr)
		}
	}
	return nil
}

// validateOverridePrefixPath checks an EgressRule override.prefix_path
// value: an absolute path fragment with no wildcard, query, or fragment
// characters, and only meaningful on allow rules.
func validateOverridePrefixPath(v, access string) error {
	if access != "allow" {
		return errors.New("only valid on allow rules")
	}
	if !strings.HasPrefix(v, "/") {
		return fmt.Errorf("must start with '/', got %q", v)
	}
	if strings.ContainsAny(v, "*?#") {
		return fmt.Errorf("must not contain wildcard, query, or fragment characters, got %q", v)
	}
	return nil
}

// pathOverlaps reports whether two mount paths collide: identical, or
// one is a parent directory of the other.
func pathOverlaps(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, strings.TrimRight(a, "/")+"/")
}
