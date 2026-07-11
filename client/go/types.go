package client

// ACLRule is one access control rule. Rules are matched longest-prefix-first;
// access is denied by default when no rule matches.
type ACLRule struct {
	// Path or glob the rule applies to (e.g. "/workspace/secret/**").
	Path string `json:"path"`
	// Access is "rw" (read-write), "ro" (read-only), or "deny".
	Access string `json:"access"`
}

// EgressOverride holds values the proxy injects into outbound requests that
// match an egress rule. If the agent already set the same query parameter or
// header, the proxy overwrites it; otherwise the value is added. The agent
// cannot read these values back.
type EgressOverride struct {
	// Host is the upstream the proxy dials instead of the matched host, as
	// "hostname[:port]" or "ip[:port]". When the port is omitted, the original
	// destination port is kept. The agent-visible request (Host header, TLS
	// SNI) keeps the original hostname.
	Host string `json:"host,omitempty"`
	// PrefixPath is prepended to the outbound request path ("/mock" turns
	// "/v1/user" into "/mock/v1/user"). The agent's original path is preserved
	// for rule matching and audit events. A trailing slash is ignored.
	PrefixPath string `json:"prefix_path,omitempty"`
	// Query holds URL query parameters to add or overwrite on the outbound
	// request. Useful for injecting API keys the agent should never see.
	Query map[string]string `json:"query,omitempty"`
	// Headers holds HTTP headers to add or overwrite on the outbound request.
	// Useful for injecting bearer tokens or tenant identifiers.
	Headers map[string]string `json:"headers,omitempty"`
	// Body, when set, rewrites the request body the proxy sends upstream. A
	// string always replaces the body verbatim. A map/struct (marshalling to a
	// JSON object) is applied per BodyStrategy: "merge" (the default)
	// shallow-merges it into the agent's JSON body — top-level keys here
	// overwrite the agent's, all other keys are preserved (agent
	// {"a":1,"b":2} with {"b":3} sends {"a":1,"b":3}); "replace" discards the
	// agent's body and sends the override object as-is. Merging applies to JSON
	// request bodies only; if the agent's body is absent or not a JSON object
	// the override object is sent as-is.
	Body any `json:"body,omitempty"`
	// BodyStrategy controls how an object Body is applied: "merge" (the
	// default) or "replace". Ignored when Body is a string.
	BodyStrategy string `json:"body_strategy,omitempty"`
}

// Body merge strategies for EgressOverride.BodyStrategy.
const (
	BodyStrategyMerge   = "merge"
	BodyStrategyReplace = "replace"
)

// EgressRule is one egress rule.
type EgressRule struct {
	// Access is "allow" or "deny" for matching requests.
	Access string `json:"access"`
	// Host is an exact host ("api.github.com") or wildcard suffix ("*.pypi.org").
	Host string `json:"host"`
	// Ports optionally restricts the rule; when empty no port enforcement is performed.
	Ports []int `json:"ports,omitempty"`
	// Methods are the HTTP methods matched by this rule. Empty means any method.
	Methods []string `json:"methods,omitempty"`
	// Paths are git-style glob path patterns matched by this rule. Empty means
	// any path. Matching is segment-by-segment on "/" boundaries. "*" matches
	// zero or more characters within a single segment and never crosses "/"
	// ("/users/*" matches "/users/42" and "/users/" but not "/users/42/posts").
	// "**" matches zero or more whole segments ("/repos/**" matches "/repos",
	// "/repos/foo", and "/repos/foo/bar"; "/a/**/z" matches "/a/z" and
	// "/a/b/c/z").
	Paths []string `json:"paths,omitempty"`
	// Override holds values the proxy injects into matching outbound requests.
	Override *EgressOverride `json:"override,omitempty"`
	// OverrideScript is an optional Lua script run against matching inspected
	// HTTP requests, after Override is applied. It can rewrite the request body
	// and headers programmatically (globals: body, headers, and read-only
	// method/host/path/query; helpers: urldecode/urlencode/b64decode/b64encode).
	OverrideScript string `json:"override_script,omitempty"`
}

// FileSystem describes a file system exposed to the agent at Mount. Backend
// selects the storage type; populate the fields for that backend. Access is
// governed by ACLs, evaluated longest-prefix-first with deny as the default.
type FileSystem struct {
	// Mount is the absolute path at which the file system appears to the agent.
	Mount string `json:"mount"`
	// Backend is "local", "gdrive", "gcs", "s3", "azure", or "external".
	Backend string `json:"backend"`
	// ACLs are access control rules for paths under Mount.
	ACLs []ACLRule `json:"acls,omitempty"`
	// Internal, when true, mounts the file system inside the sandbox runtime
	// but hides it from the agent workload. Use it for storage the sandbox
	// needs but the agent must not see, e.g. a remote-backed snapshot target
	// referenced by Snapshot.Mount. Because the agent cannot reach the mount,
	// ACLs are ignored for internal file systems.
	Internal bool `json:"internal,omitempty"`

	// Origin (local backend) is the local path to mount into this sandbox.
	// Supported only with the local Docker runtime — helpful for development,
	// e.g. mounting local skill files into the sandbox.
	Origin string `json:"origin,omitempty"`

	// GDriveAccessToken (gdrive backend) is an OAuth access token.
	GDriveAccessToken string `json:"gdrive_access_token,omitempty"`
	// GDriveRefreshToken (gdrive backend) is an OAuth refresh token.
	GDriveRefreshToken string `json:"gdrive_refresh_token,omitempty"`
	// GDriveClientID (gdrive backend) is an OAuth client ID.
	GDriveClientID string `json:"gdrive_client_id,omitempty"`
	// GDriveClientSecret (gdrive backend) is an OAuth client secret.
	GDriveClientSecret string `json:"gdrive_client_secret,omitempty"`
	// GDriveServiceAccountJSON (gdrive backend) is service account credential
	// JSON. Mutually exclusive with the OAuth fields above.
	GDriveServiceAccountJSON string `json:"gdrive_service_account_json,omitempty"`
	// GDriveFolderID (gdrive backend) scopes the file system to a Drive folder.
	// When omitted, the account root is used.
	GDriveFolderID string `json:"gdrive_folder_id,omitempty"`
	// GDrivePrefix (gdrive backend) is an optional subfolder path within
	// GDriveFolderID (e.g. "e2e-test/run-42"). Created if absent.
	GDrivePrefix string `json:"gdrive_prefix,omitempty"`

	// GCSBucket (gcs backend) is the bucket name.
	GCSBucket string `json:"gcs_bucket,omitempty"`
	// GCSPrefix (gcs backend) is an optional key prefix within the bucket
	// (e.g. "workspace/session-42"). When omitted, the bucket root is used.
	GCSPrefix string `json:"gcs_prefix,omitempty"`
	// GCSServiceAccountJSON (gcs backend) is service account credential JSON.
	// When omitted, Application Default Credentials are used.
	GCSServiceAccountJSON string `json:"gcs_service_account_json,omitempty"`

	// S3Bucket (s3 backend) is the bucket name.
	S3Bucket string `json:"s3_bucket,omitempty"`
	// S3Region (s3 backend) is the AWS region of the bucket (e.g. "us-east-1").
	S3Region string `json:"s3_region,omitempty"`
	// S3Prefix (s3 backend) is an optional key prefix within the bucket
	// (e.g. "workspace/session-42"). When omitted, the bucket root is used.
	S3Prefix string `json:"s3_prefix,omitempty"`
	// S3AccessKeyID (s3 backend) is the access key ID for the credentials.
	S3AccessKeyID string `json:"s3_access_key_id,omitempty"`
	// S3SecretAccessKey (s3 backend) is the secret access key for the credentials.
	S3SecretAccessKey string `json:"s3_secret_access_key,omitempty"`
	// S3SessionToken (s3 backend) is an optional session token for temporary
	// (STS) credentials.
	S3SessionToken string `json:"s3_session_token,omitempty"`
	// S3Endpoint (s3 backend) is an optional custom endpoint URL for
	// S3-compatible services such as MinIO, Cloudflare R2, or Backblaze B2.
	S3Endpoint string `json:"s3_endpoint,omitempty"`
	// S3UsePathStyle (s3 backend) uses path-style addressing instead of
	// virtual-hosted. Most S3-compatible services require this.
	S3UsePathStyle bool `json:"s3_use_path_style,omitempty"`

	// AzureAccount (azure backend) is the storage account name. Required
	// unless AzureConnectionString or AzureEndpoint is set.
	AzureAccount string `json:"azure_account,omitempty"`
	// AzureContainer (azure backend) is the blob container name (the Azure
	// equivalent of a bucket).
	AzureContainer string `json:"azure_container,omitempty"`
	// AzurePrefix (azure backend) is an optional key prefix within the
	// container (e.g. "workspace/session-42"). When omitted, the container
	// root is used.
	AzurePrefix string `json:"azure_prefix,omitempty"`
	// AzureAccountKey (azure backend) is the storage account access key
	// (shared-key auth). One of AzureAccountKey, AzureConnectionString, or
	// AzureSASToken is required.
	AzureAccountKey string `json:"azure_account_key,omitempty"`
	// AzureConnectionString (azure backend) is a full connection string
	// (account, key, and endpoint). Takes precedence over the other
	// credential fields.
	AzureConnectionString string `json:"azure_connection_string,omitempty"`
	// AzureSASToken (azure backend) is a shared access signature token
	// authorizing the container. A leading "?" is optional.
	AzureSASToken string `json:"azure_sas_token,omitempty"`
	// AzureEndpoint (azure backend) is an optional custom blob service
	// endpoint (e.g. the Azurite emulator). When omitted,
	// "https://{account}.blob.core.windows.net" is used.
	AzureEndpoint string `json:"azure_endpoint,omitempty"`

	// Host (external backend) is the base URL of the host you implement to back
	// this file system. A trailing slash is ignored.
	Host string `json:"host,omitempty"`
}

// Snapshot configures sandbox snapshots. It has two independent parts: VM
// captures the full microVM state (a no-op for the container backend) and Files
// captures the writable filesystem as a portable tarball. Either may be present
// alone, both, or neither. Snapshots are captured by the snapshot action (and,
// for Files, optionally on shutdown) and restored when the sandbox starts.
type Snapshot struct {
	// VM names the microVM-state snapshot. A get-or-create resumes the keyed VM
	// snapshot if one exists, else cold-boots. Ignored by the container backend.
	VM *SnapshotVM `json:"vm,omitempty"`
	// Files names the writable-filesystem snapshot.
	Files *SnapshotFiles `json:"files,omitempty"`
}

// SnapshotVM names a microVM-state snapshot.
type SnapshotVM struct {
	// Key identifies the VM-state snapshot.
	Key string `json:"key"`
}

// SnapshotFiles names a writable-filesystem snapshot and what it covers.
type SnapshotFiles struct {
	// Key identifies the files snapshot.
	Key string `json:"key"`
	// Include are glob patterns for the paths to capture (e.g. "/home/user/*").
	Include []string `json:"include,omitempty"`
	// WriteOnShutdown, when true, captures the files snapshot on shutdown or
	// termination. When false (the default), files are captured only by an
	// explicit snapshot action.
	WriteOnShutdown bool `json:"write_on_shutdown,omitempty"`
	// Mount is the mount path of a FileSystem (see SandboxConfig.FS) where the
	// files tarball is written and read, instead of the host's local snapshot
	// directory. Point it at an Internal, remote-backed file system to persist
	// and restore through a FUSE drive.
	Mount string `json:"mount,omitempty"`
}

// SnapshotResult is the outcome of a Snapshot action, reported per requested
// part. A part the request omitted is nil.
type SnapshotResult struct {
	VM    *SnapshotPartResult `json:"vm,omitempty"`
	Files *SnapshotPartResult `json:"files,omitempty"`
}

// SnapshotPartResult is the outcome of capturing one snapshot part.
type SnapshotPartResult struct {
	// Captured reports whether the part was captured. False (with Reason) when
	// the part is unsupported on the active backend, e.g. VM on a container.
	Captured bool `json:"captured"`
	// Key is the key the part was written under.
	Key string `json:"key"`
	// Bytes is the captured artifact size in bytes, when known.
	Bytes int64 `json:"bytes,omitempty"`
	// Reason explains why the part was not captured, when Captured is false.
	Reason string `json:"reason,omitempty"`
}

// SandboxConfig is the configuration for a sandbox.
type SandboxConfig struct {
	// Image references the agent image to launch. Cannot be changed after the
	// sandbox is initialized.
	Image string `json:"image,omitempty"`
	// CPU is the number of virtual CPUs allocated to the sandbox, as a ceiling
	// (the pod CPU limit). May be fractional (e.g. 0.5); the microvm guest vCPU
	// count is this value rounded up. Defaults to 1. Cannot be changed after the
	// sandbox is initialized.
	CPU float64 `json:"cpu,omitempty"`
	// Memory allocated to the sandbox, in MiB. Defaults to 512. Cannot be
	// changed after the sandbox is initialized.
	Memory int `json:"memory,omitempty"`
	// Entrypoint overrides the entrypoint used when the sandbox is run. Accepts
	// either a []string argv (each element a separate argument) or a single
	// string, which the sandbox splits on whitespace into arguments. When
	// omitted, the image's default entrypoint is used.
	Entrypoint any `json:"entrypoint,omitempty"`
	// CWD is the working directory for the entrypoint. When omitted, the
	// image's working directory is used. Cannot be changed after the sandbox
	// is initialized.
	CWD string `json:"cwd,omitempty"`
	// TTY launches the entrypoint attached to a pseudo-TTY; attach to it with
	// ExecStream and an empty command. Container isolation only. Cannot be
	// changed after the sandbox is initialized.
	TTY bool `json:"tty,omitempty"`
	// Env holds additional environment variables.
	Env map[string]string `json:"env,omitempty"`
	// ExtraHosts holds additional /etc/hosts entries in "hostname:ip" form (use
	// "host-gateway" for the host machine's IP). Cannot be changed after the
	// sandbox is initialized.
	ExtraHosts []string `json:"extra_hosts,omitempty"`
	// TTL is the sandbox time to live in seconds. Call Sandbox.Ping to reset
	// the timer; once a ping has not been received for this long the sandbox is
	// stopped. Defaults to 1800 (30 min). It is a pointer so callers can set 0
	// to disable shutdown.
	TTL *int `json:"ttl,omitempty"`
	// FS holds the file systems exposed to the agent. Mount paths must be
	// unique and non-overlapping.
	FS []FileSystem `json:"fs,omitempty"`
	// Egress is the ordered list of egress rules. The first rule that matches a
	// request decides the outcome; requests that match no rule are denied.
	Egress []EgressRule `json:"egress,omitempty"`
	// Snapshot configures automatic snapshots for this sandbox.
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

// SandboxInfo is internal runtime information about a sandbox, determined at
// boot rather than configured. Read via Sandbox.GetInfo.
type SandboxInfo struct {
	// Isolation is the mechanism the sandbox is running with: "container" or
	// "microvm". It is selected automatically from the image — a microvm image
	// ships a guest root filesystem — not from config.
	Isolation string `json:"isolation"`
}

// SandboxRef is a provisioned sandbox handle returned by the controller.
type SandboxRef struct {
	// ID is the server-assigned unique identifier (uuid).
	ID string `json:"id"`
	// Key is the caller-chosen key the sandbox was provisioned under.
	Key string `json:"key"`
}

// APIError is the structured error returned by the server.
type APIError struct {
	// Message is the human-readable failure reason.
	Message string `json:"error"`
	// Details is optional structured context such as the offending field path.
	Details map[string]interface{} `json:"details,omitempty"`
}

func (e *APIError) Error() string { return e.Message }

// FSChanges lists the file systems added or removed by an apply.
type FSChanges struct {
	Added   []FileSystem `json:"added,omitempty"`
	Removed []FileSystem `json:"removed,omitempty"`
}

// EgressChanges lists the egress rules added or removed by an apply.
type EgressChanges struct {
	Added   []EgressRule `json:"added,omitempty"`
	Removed []EgressRule `json:"removed,omitempty"`
}

// Changes describes the concrete additions and removals carried out by an
// apply, so the caller can audit what changed without re-diffing the request.
type Changes struct {
	// FS holds file systems added or removed.
	FS *FSChanges `json:"fs,omitempty"`
	// Egress holds egress rules added or removed.
	Egress *EgressChanges `json:"egress,omitempty"`
	// Warnings are non-fatal advisories, e.g. a non-modifiable field was
	// present in the request and was ignored.
	Warnings []string `json:"warnings,omitempty"`
}

// ApplyResult is the outcome of an apply. The change is all-or-nothing.
type ApplyResult struct {
	// Applied is true if every change was committed; false if the apply failed
	// and was rolled back, leaving the sandbox unchanged.
	Applied bool `json:"applied"`
	// Config is the configuration in effect after this call.
	Config SandboxConfig `json:"config"`
	// Changes details what was added or removed.
	Changes Changes `json:"changes"`
	// Error is the human-readable failure reason. Set only when Applied is false.
	Error string `json:"error,omitempty"`
}

// ExecRequest runs a command inside the sandbox and buffers its output.
type ExecRequest struct {
	// Command is the command to run. A string is passed to a shell (sh -c); a
	// []string is executed directly as argv, with each element a literal
	// argument (no shell, no word-splitting or expansion).
	Command any `json:"command"`
	// CWD is the working directory to run in. When empty, the sandbox's
	// working directory is used.
	CWD string `json:"cwd,omitempty"`
	// Env is merged on top of the sandbox config's environment, overriding
	// entries with the same name.
	Env map[string]string `json:"env,omitempty"`
}

// ExecResult is the buffered result of a command, available once it exits.
type ExecResult struct {
	// Stdout is everything the command wrote to stdout.
	Stdout string `json:"stdout"`
	// Stderr is everything the command wrote to stderr.
	Stderr string `json:"stderr"`
	// ExitCode is the command's process exit code.
	ExitCode int `json:"exit_code"`
}

// ExecStreamRequest runs a command with streamed I/O. Leave Command empty to
// attach to the sandbox entrypoint's terminal (requires the sandbox to have
// been created with TTY true).
type ExecStreamRequest struct {
	// Command is the command to run; empty/nil attaches to the entrypoint
	// terminal. A string is passed to a shell (sh -c); a []string is executed
	// directly as argv, with each element a literal argument (no shell).
	Command any `json:"command,omitempty"`
	// CWD is the working directory to run in. When empty, the sandbox's
	// working directory is used.
	CWD string `json:"cwd,omitempty"`
	// Env is merged on top of the sandbox config's environment, overriding
	// entries with the same name.
	Env map[string]string `json:"env,omitempty"`
	// TTY allocates a pseudo-TTY so interactive programs behave as in a terminal.
	TTY bool `json:"tty,omitempty"`
}

// ExecOutput is one output frame from a streaming exec.
type ExecOutput struct {
	// Stdout is a chunk of stdout output; non-empty on stdout frames.
	Stdout string
	// Stderr is a chunk of stderr output; non-empty on stderr frames.
	Stderr string
}

// DirEntry is one entry in a directory listing.
type DirEntry struct {
	// Name is the entry's base name.
	Name string `json:"name"`
	// Path is the agent-visible absolute path of the entry.
	Path string `json:"path"`
	// IsDir reports whether the entry is a directory.
	IsDir bool `json:"is_dir"`
	// Size is the entry size in bytes.
	Size int64 `json:"size"`
}

// UploadResult reports where an uploaded file landed.
type UploadResult struct {
	// Path is the agent-visible path the file was written to.
	Path string `json:"path"`
	// Bytes is the number of bytes written.
	Bytes int64 `json:"bytes"`
}

// SandboxLifecycleEvent is a lifecycle change observed for a single sandbox.
type SandboxLifecycleEvent struct {
	// ID is the server-assigned unique identifier (uuid).
	ID string `json:"id"`
	// Key is the caller-chosen key the sandbox was provisioned under.
	Key string `json:"key"`
	// Status is the transition that occurred: "start", "stop", "die", or "destroy".
	Status string `json:"status"`
}

// SandboxEvent is a single activity event from a sandbox. Inspect Type to
// determine which fields are populated.
type SandboxEvent struct {
	// ID is a monotonic event id, usable as a resume cursor.
	ID int `json:"id"`
	// Timestamp is when the event occurred, as an ISO-8601 string.
	Timestamp string `json:"timestamp"`
	// Type discriminates the event variant.
	Type string `json:"type"`

	// config.apply
	Success      bool     `json:"success,omitempty"`
	Changes      *Changes `json:"changes,omitempty"`
	ErrorMessage string   `json:"errorMessage,omitempty"`

	// egress.request, ingress.request
	Access  string            `json:"access,omitempty"`
	Host    string            `json:"host,omitempty"`
	Method  string            `json:"method,omitempty"`
	Path    string            `json:"path,omitempty"`
	Query   string            `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Port    string            `json:"port,omitempty"` // ingress only

	// egress.response, ingress.response
	RequestID  int `json:"request_id,omitempty"`
	Status     int `json:"status,omitempty"`
	DurationMs int `json:"duration_ms,omitempty"`

	// egress.chunk
	Label string `json:"label,omitempty"` // "up" for client→upstream, "down" for upstream→client (WebSocket only)

	// fs.request
	Mount     string `json:"mount,omitempty"`
	Operation string `json:"operation,omitempty"` // "read", "write", or "delete"

	// fs.response
	Backend string `json:"backend,omitempty"`
	Error   string `json:"error,omitempty"`

	// stdio
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`

	// resource.usage
	CPUPercent  float64 `json:"cpu_percent,omitempty"`
	MemoryBytes int64   `json:"memory_bytes,omitempty"`

	// exec.request, exec.response
	CWD     string `json:"cwd,omitempty"`
	Command string `json:"command,omitempty"`
}
