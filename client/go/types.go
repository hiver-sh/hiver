package client

// ACLRule is one access control rule for a filesystem mount.
type ACLRule struct {
	Path   string `json:"path"`
	Access string `json:"access"` // "rw", "ro", or "deny"
}

// EgressOverride specifies values the proxy injects into outbound requests.
// Host ("hostname[:port]") substitutes the upstream the proxy dials for
// matching requests; matching and the agent-visible request (Host header,
// SNI) keep the original hostname. When the port is omitted, the original
// destination port is kept. PrefixPath ("/mock") is prepended to the
// outbound request path; matching and audit events keep the original path.
type EgressOverride struct {
	Host       string            `json:"host,omitempty"`
	PrefixPath string            `json:"prefix_path,omitempty"`
	Query      map[string]string `json:"query,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
}

// EgressRule is one egress rule.
type EgressRule struct {
	Access   string          `json:"access"` // "allow" or "deny"
	Host     string          `json:"host"`
	Ports    []int           `json:"ports,omitempty"`
	Methods  []string        `json:"methods,omitempty"`
	Paths    []string        `json:"paths,omitempty"`
	Override *EgressOverride `json:"override,omitempty"`
}

// FileSystem describes a filesystem exposed to the sandbox.
// Set Backend to "local", "gdrive", or "gcs" and populate the corresponding fields.
type FileSystem struct {
	Mount   string    `json:"mount"`
	Backend string    `json:"backend"` // "local", "gdrive", "gcs", or "external"
	ACLs    []ACLRule `json:"acls,omitempty"`

	// local backend
	Origin string `json:"origin,omitempty"`

	// gdrive backend
	GDriveAccessToken        string `json:"gdrive_access_token,omitempty"`
	GDriveRefreshToken       string `json:"gdrive_refresh_token,omitempty"`
	GDriveClientID           string `json:"gdrive_client_id,omitempty"`
	GDriveClientSecret       string `json:"gdrive_client_secret,omitempty"`
	GDriveServiceAccountJSON string `json:"gdrive_service_account_json,omitempty"`
	GDriveFolderID           string `json:"gdrive_folder_id,omitempty"`
	GDrivePrefix             string `json:"gdrive_prefix,omitempty"`

	// gcs backend
	GCSBucket             string `json:"gcs_bucket,omitempty"`
	GCSPrefix             string `json:"gcs_prefix,omitempty"`
	GCSServiceAccountJSON string `json:"gcs_service_account_json,omitempty"`

	// external backend — base URL of the HTTP host implementing the
	// external file system contract (api/external_file_system.yaml).
	Host string `json:"host,omitempty"`
}

// Snapshot configures automatic sandbox snapshots.
type Snapshot struct {
	RestoreKey string   `json:"restore_key,omitempty"`
	WriteKey   string   `json:"write_key,omitempty"`
	Include    []string `json:"include,omitempty"`
}

// SandboxConfig is the configuration document for a sandbox.
type SandboxConfig struct {
	Image      string            `json:"image,omitempty"`
	Isolation  string            `json:"isolation,omitempty"` // "container" or "microvm"
	CPU        int               `json:"cpu,omitempty"`
	Memory     int               `json:"memory,omitempty"`
	Entrypoint string            `json:"entrypoint,omitempty"`
	CWD        string            `json:"cwd,omitempty"`
	TTY        bool              `json:"tty,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	ExtraHosts []string          `json:"extra_hosts,omitempty"`
	TTL        *int              `json:"ttl,omitempty"` // pointer so callers can set 0 (disable TTL)
	FS         []FileSystem      `json:"fs,omitempty"`
	Egress     []EgressRule      `json:"egress,omitempty"`
	Snapshot   *Snapshot         `json:"snapshot,omitempty"`
}

// SandboxRef is the minimal reference returned by the controller.
type SandboxRef struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// APIError is the error body returned by the server.
type APIError struct {
	Message string                 `json:"error"`
	Details map[string]interface{} `json:"details,omitempty"`
}

func (e *APIError) Error() string { return e.Message }

// FSChanges describes filesystem changes from an apply.
type FSChanges struct {
	Added   []FileSystem `json:"added,omitempty"`
	Removed []FileSystem `json:"removed,omitempty"`
}

// EgressChanges describes egress rule changes from an apply.
type EgressChanges struct {
	Added   []EgressRule `json:"added,omitempty"`
	Removed []EgressRule `json:"removed,omitempty"`
}

// Changes describes the concrete additions and removals carried out by an apply.
type Changes struct {
	FS       *FSChanges     `json:"fs,omitempty"`
	Egress   *EgressChanges `json:"egress,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
}

// ApplyResult is the response from PUT /v1/config.
type ApplyResult struct {
	Applied bool          `json:"applied"`
	Config  SandboxConfig `json:"config"`
	Changes Changes       `json:"changes"`
	Error   string        `json:"error,omitempty"`
}

// ExecRequest is the body for POST /v1/exec.
type ExecRequest struct {
	Command string            `json:"command"`
	CWD     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// ExecResult is the response from POST /v1/exec.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// ExecStreamRequest is the body for POST /v1/exec-stream/{id}.
// Leave Command empty to attach to the sandbox entrypoint's TTY.
type ExecStreamRequest struct {
	Command string            `json:"command"` // required by the server schema; empty = attach to entrypoint TTY
	CWD     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	TTY     bool              `json:"tty,omitempty"`
}

// ExecOutput is one stdout or stderr frame from a streaming exec.
type ExecOutput struct {
	Stdout string // non-empty on stdout frames
	Stderr string // non-empty on stderr frames
}

// DirEntry is one entry in a directory listing.
type DirEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// UploadResult is the response from POST /v1/file.
type UploadResult struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// SandboxLifecycleEvent is a controller-side lifecycle event.
type SandboxLifecycleEvent struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Status string `json:"status"` // "start", "stop", "die", or "destroy"
}

// SandboxEvent is a sandbox-side audit event. Check Type to determine which
// fields are populated.
type SandboxEvent struct {
	ID        int    `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`

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
	Label string `json:"label,omitempty"`

	// fs.request
	Mount     string `json:"mount,omitempty"`
	Operation string `json:"operation,omitempty"` // "read" or "write"

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
