// Package spec defines the on-wire format that drives sandboxd.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/blasten/hive/internal/fusefs"
	"github.com/blasten/hive/internal/proxy"
)

var snapshotKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// BackendSuffix is appended to a mount path to derive the host-side
// backend directory (e.g. "/workspace" → "/workspace-backend").
const BackendSuffix = "-backend"

// Spec is the root document. Loaded by sandboxd via [Load].
type Spec struct {
	Image      string             `json:"image,omitempty"`
	Isolation  Isolation          `json:"isolation,omitempty"`
	CPU        *int               `json:"cpu,omitempty"`
	Memory     *int               `json:"memory,omitempty"`
	Entrypoint string             `json:"entrypoint,omitempty"`
	Ttl        *int               `json:"ttl,omitempty"`
	Env        map[string]string  `json:"env,omitempty"`
	FS         []FS               `json:"fs"`
	Egress     []proxy.EgressRule `json:"egress,omitempty"`
	Snapshot   *Snapshot          `json:"snapshot,omitempty"`
}

// Isolation selects how the agent workload is confined. It mirrors
// SandboxConfig.isolation in the API schema; the matching backends live in
// [internal/isolation]. The empty value means "container" (runc) so configs
// that predate the field keep their behaviour.
type Isolation string

const (
	IsolationContainer Isolation = "container"
	IsolationMicroVM   Isolation = "microvm"
)

// Valid reports whether the isolation is one sandboxd knows how to wire up.
// The empty string is valid and resolves to [IsolationContainer].
func (i Isolation) Valid() bool {
	switch i {
	case "", IsolationContainer, IsolationMicroVM:
		return true
	}
	return false
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

	// gcs only — bucket, optional key prefix, and optional service account.
	GcsBucket             string `json:"gcs_bucket,omitempty"`
	GcsPrefix             string `json:"gcs_prefix,omitempty"`
	GcsServiceAccountJSON string `json:"gcs_service_account_json,omitempty"`
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
)

// Valid reports whether the backend is one sandboxd knows how to wire up.
func (b Backend) Valid() bool {
	switch b {
	case BackendLocal, BackendGoogleDrive, BackendGoogleCloudStorage:
		return true
	}
	return false
}

// IsRemote reports whether the backend writes through to a remote
// store via the oplog. Local is the only non-remote backend today.
func (b Backend) IsRemote() bool {
	switch b {
	case BackendGoogleDrive, BackendGoogleCloudStorage:
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
)

// Env-var fallbacks for gcs credentials.
const (
	envGcsBucket             = "HIVE_GCS_BUCKET"
	envGcsPrefix             = "HIVE_GCS_PREFIX"
	envGcsServiceAccountJSON = "HIVE_GCS_SERVICE_ACCOUNT_JSON"
)

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
func (f *FS) gdriveResolved() (accessToken, refreshToken, clientID, clientSecret, serviceAccountJSON, folderID string) {
	return or(f.GdriveAccessToken, envGdriveAccessToken),
		or(f.GdriveRefreshToken, envGdriveRefreshToken),
		or(f.GdriveClientID, envGdriveClientID),
		or(f.GdriveClientSecret, envGdriveClientSecret),
		or(f.GdriveServiceAccountJSON, envGdriveServiceAccountJSON),
		or(f.GdriveFolderID, envGdriveFolderID)
}

// gcsResolved returns the effective gcs config — spec fields with env-var fallback.
func (f *FS) gcsResolved() (bucket, prefix, serviceAccountJSON string) {
	return or(f.GcsBucket, envGcsBucket),
		or(f.GcsPrefix, envGcsPrefix),
		or(f.GcsServiceAccountJSON, envGcsServiceAccountJSON)
}

// BackendConfigJSON returns the per-backend config sandboxd should hand
// to sbxfuse via -remote-config. Returns (nil, nil) for backends that
// take no config (local). The schema mirrors the matching
// [internal/remotefs] config struct so sbxfuse can json.Unmarshal
// directly without translation.
func (f *FS) BackendConfigJSON() ([]byte, error) {
	switch f.Backend {
	case BackendGoogleDrive:
		access, refresh, clientID, clientSecret, sa, folder := f.gdriveResolved()
		return json.Marshal(struct {
			AccessToken        string `json:"access_token,omitempty"`
			RefreshToken       string `json:"refresh_token,omitempty"`
			ClientID           string `json:"client_id,omitempty"`
			ClientSecret       string `json:"client_secret,omitempty"`
			ServiceAccountJSON string `json:"service_account_json,omitempty"`
			FolderID           string `json:"folder_id,omitempty"`
		}{
			AccessToken:        access,
			RefreshToken:       refresh,
			ClientID:           clientID,
			ClientSecret:       clientSecret,
			ServiceAccountJSON: sa,
			FolderID:           folder,
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
	if !s.Isolation.Valid() {
		return fmt.Errorf("isolation: unknown value %q (supported: %q, %q)", s.Isolation, IsolationContainer, IsolationMicroVM)
	}
	if s.CPU != nil && *s.CPU < 1 {
		return fmt.Errorf("cpu: must be >= 1, got %d", *s.CPU)
	}
	if s.Memory != nil && *s.Memory < 128 {
		return fmt.Errorf("memory: must be >= 128 (MiB), got %d", *s.Memory)
	}
	if len(s.FS) == 0 {
		return errors.New("fs is required (at least one mount)")
	}
	for i := range s.FS {
		f := &s.FS[i]
		ctx := fmt.Sprintf("fs[%d]", i)
		if f.Backend == "" {
			return fmt.Errorf("%s.backend is required", ctx)
		}
		if !f.Backend.Valid() {
			return fmt.Errorf("%s.backend: unknown value %q (supported: %q, %q, %q)", ctx, f.Backend, BackendLocal, BackendGoogleDrive, BackendGoogleCloudStorage)
		}
		if f.Backend == BackendGoogleDrive {
			access, _, _, _, sa, _ := f.gdriveResolved()
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
	}
	if sn := s.Snapshot; sn != nil {
		if sn.RestoreKey != "" && !snapshotKeyRE.MatchString(sn.RestoreKey) {
			return fmt.Errorf("snapshot.restore_key: must match %s", snapshotKeyRE)
		}
		if sn.WriteKey != "" && !snapshotKeyRE.MatchString(sn.WriteKey) {
			return fmt.Errorf("snapshot.write_key: must match %s", snapshotKeyRE)
		}
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
