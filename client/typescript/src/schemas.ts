import { z } from "zod";

/**
 * Storage type for a file system.
 * - `local`  — sandbox-local storage with no external dependency.
 * - `gdrive` — backed by Google Drive.
 * - `gcs`    — backed by Google Cloud Storage.
 */
export const Backend = z.enum(["local", "gdrive", "gcs"]);
export type Backend = z.infer<typeof Backend>;

/** One access control rule. */
export const ACLRule = z.object({
  /** Path or glob the rule applies to (e.g. `/workspace/secret/**`). Rules are matched longest-prefix-first; access is denied by default when no rule matches. */
  path: z.string(),
  /** Read-write, read-only, or fully denied. */
  access: z.enum(["rw", "ro", "deny"]),
});
export type ACLRule = z.infer<typeof ACLRule>;

const FileSystemBase = z.object({
  /** Absolute path at which the file system appears to the agent. */
  mount: z.string().regex(/^\/.+/, "mount must be an absolute path"),
  /** Access control rules for paths under `mount`. */
  acls: z.array(ACLRule).optional(),
});

/** Sandbox-local storage with no external dependency. */
export const LocalFileSystem = FileSystemBase.extend({
  backend: z.literal("local"),
  /**
   * The local path to mount into this sandbox.
   * Local origins are only supported locally with the Docker runtime.
   * Helpful for local development, e.g. mounting local skill files into the sandbox.
   */
  origin: z.string().optional(),
});
export type LocalFileSystem = z.infer<typeof LocalFileSystem>;

/** A file system backed by Google Drive. */
export const GDriveFileSystem = FileSystemBase.extend({
  backend: z.literal("gdrive"),
  /** OAuth access token. */
  gdrive_access_token: z.string().optional(),
  /** OAuth refresh token. */
  gdrive_refresh_token: z.string().optional(),
  /** OAuth client ID. */
  gdrive_client_id: z.string().optional(),
  /** OAuth client secret. */
  gdrive_client_secret: z.string().optional(),
  /** Service account credential JSON. Mutually exclusive with the OAuth fields above. */
  gdrive_service_account_json: z.string().optional(),
  /** ID of the Drive folder the file system is scoped to. When omitted, the account root is used. */
  gdrive_folder_id: z.string().optional(),
  /** Optional subfolder path within gdrive_folder_id (e.g. `e2e-test/run-42`). Created if absent. */
  gdrive_prefix: z.string().optional(),
});
export type GDriveFileSystem = z.infer<typeof GDriveFileSystem>;

/** A file system backed by Google Cloud Storage. */
export const GCSFileSystem = FileSystemBase.extend({
  backend: z.literal("gcs"),
  /** GCS bucket name. */
  gcs_bucket: z.string(),
  /** Optional key prefix within the bucket (e.g. `workspace/session-42`). When omitted, the bucket root is used. */
  gcs_prefix: z.string().optional(),
  /**
   * Service account credential JSON. When omitted, Application Default Credentials are used
   * (GOOGLE_APPLICATION_CREDENTIALS env var, gcloud user credentials, or the GCE/GKE metadata server).
   */
  gcs_service_account_json: z.string(),
});
export type GCSFileSystem = z.infer<typeof GCSFileSystem>;

/**
 * A file system exposed to the agent at `mount`. The `backend` selects the storage type and
 * determines which variant applies. Access is governed by `acls`, evaluated longest-prefix-first
 * with deny as the default.
 */
export const FileSystem = z.discriminatedUnion("backend", [
  LocalFileSystem,
  GDriveFileSystem,
  GCSFileSystem,
]);
export type FileSystem = z.infer<typeof FileSystem>;

export const HttpMethod = z.enum([
  "GET",
  "POST",
  "PUT",
  "PATCH",
  "DELETE",
  "HEAD",
  "OPTIONS",
]);
export type HttpMethod = z.infer<typeof HttpMethod>;

/**
 * Values the proxy injects into outbound requests that match an egress rule.
 * If the agent already set the same query parameter or header, the proxy overwrites it;
 * otherwise the value is added. The agent cannot read these values back.
 */
export const EgressOverride = z.object({
  /** URL query parameters to add or overwrite on the outbound request. Useful for injecting API keys the agent should never see. */
  query: z.record(z.string(), z.string()).optional(),
  /** HTTP headers to add or overwrite on the outbound request. Useful for injecting bearer tokens or tenant identifiers. */
  headers: z.record(z.string(), z.string()).optional(),
});
export type EgressOverride = z.infer<typeof EgressOverride>;

/** One egress rule. */
export const EgressRule = z.object({
  /** Whether matching requests are allowed or denied. */
  access: z.enum(["allow", "deny"]),
  /** Exact host (`api.github.com`) or wildcard suffix (`*.pypi.org`). */
  host: z.string(),
  /** Optional ports; when omitted no port enforcement is performed. */
  ports: z.array(z.number().int()).optional(),
  /** HTTP methods matched by this rule. Empty means any method. */
  methods: z.array(HttpMethod).optional(),
  /** Glob path patterns matched by this rule. Empty means any path. */
  paths: z.array(z.string()).optional(),
  override: EgressOverride.optional(),
});
export type EgressRule = z.infer<typeof EgressRule>;

/**
 * Snapshot configuration. A snapshot is captured automatically before the sandbox shuts down
 * and restored before the sandbox starts.
 */
export const Snapshot = z.object({
  /** Key identifying the snapshot to restore when the sandbox starts. When omitted, no snapshot is restored on start. */
  restore_key: z
    .string()
    .regex(/^[A-Za-z0-9_-]{1,64}$/)
    .optional(),
  /** Key under which the snapshot is saved on shutdown. When omitted, `restore_key` is used. */
  write_key: z
    .string()
    .regex(/^[A-Za-z0-9_-]{1,64}$/)
    .optional(),
  /** Glob patterns specifying which paths to include in the snapshot (e.g. `/home/user/*`). */
  include: z.array(z.string()).min(1).optional(),
});
export type Snapshot = z.infer<typeof Snapshot>;

/** Hive sandbox configuration. */
export const SandboxConfig = z.object({
  /** Reference to the agent image to launch. This cannot be changed after the sandbox is initialized. */
  image: z.string().optional(),
  /** The isolation mechanism used to run the sandbox. This cannot be changed after the sandbox is initialized. */
  isolation: z.enum(["container", "microvm"]).optional(),
  /**
   * Number of virtual CPUs allocated to the sandbox (microvm: guest vCPU count;
   * container: enforced as a CPU quota). Defaults to 1.
   * This cannot be changed after the sandbox is initialized.
   */
  cpu: z.number().int().min(1).optional(),
  /**
   * Memory allocated to the sandbox, in MiB (microvm: guest RAM size;
   * container: enforced as a cgroup memory limit). Defaults to 512.
   * This cannot be changed after the sandbox is initialized.
   */
  memory: z.number().int().min(128).optional(),
  /** Override the entrypoint used when the container is run. When omitted, the image's default entrypoint is used. */
  entrypoint: z.string().optional(),
  /**
   * Working directory for the entrypoint. When set, the entrypoint is launched
   * with this as its current working directory, overriding the image's default
   * working directory. When omitted, the image's working directory is used.
   * This cannot be changed after the sandbox is initialized.
   */
  cwd: z.string().optional(),
  /**
   * When true, the entrypoint is launched attached to a pseudo-TTY. A client can
   * then attach to that terminal by calling `execStream` with an empty command.
   * Only supported for the `container` isolation. Defaults to false.
   * This cannot be changed after the sandbox is initialized.
   */
  tty: z.boolean().optional(),
  /** Additional environment variables in `KEY=VALUE` form. This cannot be changed after the sandbox is initialized. */
  env: z.record(z.string(), z.string()).optional(),
  /** Additional /etc/hosts entries in `hostname:ip` form (use `host-gateway` for the host machine's IP). Cannot be changed after the sandbox is initialized. */
  extra_hosts: z.array(z.string()).optional(),
  /**
   * Sandbox time to live in seconds. The client must ping `/v1/ping` to reset the timer;
   * once a ping has not been received for this long the sandbox receives SIGTERM.
   * Defaults to 1800 (30 min). Use `0` to disable shutdown.
   */
  ttl: z.number().int().min(0).optional(),
  /**
   * One or more file systems exposed to the agent. Mount paths must be unique and
   * non-overlapping (a mount path may not be a parent directory of another mount path).
   */
  fs: z.array(FileSystem).min(1).optional(),
  /**
   * Ordered list of egress rules. The first rule that matches a request decides the outcome;
   * requests that match no rule are denied.
   */
  egress: z.array(EgressRule).optional(),
  snapshot: Snapshot.optional(),
});
export type SandboxConfig = z.infer<typeof SandboxConfig>;

/**
 * Concrete additions and removals carried out by an apply call. Each list contains whole entries
 * (a complete `FileSystem` or `EgressRule`) so the caller can audit what changed without
 * re-diffing the request.
 */
export const Changes = z.object({
  fs: z
    .object({
      added: z.array(FileSystem).optional(),
      removed: z.array(FileSystem).optional(),
    })
    .optional(),
  egress: z
    .object({
      added: z.array(EgressRule).optional(),
      removed: z.array(EgressRule).optional(),
    })
    .optional(),
  /** Non-fatal advisories. Example: a non-modifiable field was present in the request and was ignored. */
  warnings: z.array(z.string()).optional(),
});
export type Changes = z.infer<typeof Changes>;

/** The outcome of an apply call. */
export const ApplyResult = z.object({
  /** `true` if every change was applied successfully. `false` if the apply failed and was rolled back; in that case the sandbox is unchanged. */
  applied: z.boolean(),
  /**
   * The configuration in effect after this call. When `applied` is `true` this matches the
   * request, except for any non-modifiable fields preserved from the previous state
   * (see `changes.warnings`).
   */
  config: SandboxConfig,
  changes: Changes,
  /** Human-readable failure reason. Set only when `applied` is `false`. */
  error: z.string().optional(),
});
export type ApplyResult = z.infer<typeof ApplyResult>;

export const ApiError = z.object({
  /** Human-readable failure reason. */
  error: z.string(),
  /** Optional structured context such as the offending field path or a conflict identifier. */
  details: z.record(z.string(), z.unknown()).optional(),
});
export type ApiError = z.infer<typeof ApiError>;

/** Controller response: a provisioned sandbox handle. */
export const SandboxRef = z.object({
  /** Server-assigned unique identifier (uuid). */
  id: z.string(),
  /** Caller-chosen key the sandbox was provisioned under; used for routing. */
  key: z.string(),
});
export type SandboxRef = z.infer<typeof SandboxRef>;

const SandboxEventBase = z.object({
  id: z.number().int(),
  timestamp: z.string(),
});

export const ConfigApplyEvent = SandboxEventBase.extend({
  type: z.literal("config.apply"),
  success: z.boolean(),
  changes: Changes,
  errorMessage: z.string().optional(),
});

export const EgressRequestEvent = SandboxEventBase.extend({
  type: z.literal("egress.request"),
  access: z.enum(["allowed", "denied"]),
  host: z.string(),
  method: z.string(),
  path: z.string(),
  query: z.string().optional(),
  headers: z.record(z.string(), z.string()).optional(),
  body: z.string().optional(),
});

export const EgressResponseEvent = SandboxEventBase.extend({
  type: z.literal("egress.response"),
  request_id: z.number(),
  status: z.number().int(),
  duration_ms: z.number().int(),
  headers: z.record(z.string(), z.string()).optional(),
});

export const EgressChunkEvent = SandboxEventBase.extend({
  type: z.literal("egress.chunk"),
  request_id: z.number(),
  body: z.string(),
  /** Optional origin tag: `up` for client→upstream, `down` for upstream→client (WebSocket only). */
  label: z.string().optional(),
});

export const FSRequestEvent = SandboxEventBase.extend({
  type: z.literal("fs.request"),
  access: z.enum(["allowed", "denied"]),
  mount: z.string(),
  path: z.string(),
  operation: z.enum(["read", "write"]),
});

export const FSResponseEvent = SandboxEventBase.extend({
  type: z.literal("fs.response"),
  backend: Backend,
  request_id: z.number(),
  duration_ms: z.number().int(),
  error: z.string().optional(),
});

export const StdioEvent = SandboxEventBase.extend({
  type: z.literal("stdio"),
  stdout: z.string().optional(),
  stderr: z.string().optional(),
});

export const ResourceUsageEvent = SandboxEventBase.extend({
  type: z.literal("resource.usage"),
  cpu_percent: z.number(),
  memory_bytes: z.number().int(),
});

export const ExecRequestEvent = SandboxEventBase.extend({
  type: z.literal("exec.request"),
  cwd: z.string(),
  command: z.string(),
});

export const ExecResponseEvent = SandboxEventBase.extend({
  type: z.literal("exec.response"),
  request_id: z.number().int(),
});

export const IngressRequestEvent = SandboxEventBase.extend({
  type: z.literal("ingress.request"),
  port: z.string(),
  method: z.string(),
  path: z.string(),
  query: z.string().optional(),
  headers: z.record(z.string(), z.string()).optional(),
  body: z.string().optional(),
});

export const IngressResponseEvent = SandboxEventBase.extend({
  type: z.literal("ingress.response"),
  request_id: z.number().int(),
  status: z.number().int(),
  duration_ms: z.number().int(),
  headers: z.record(z.string(), z.string()).optional(),
  body: z.string().optional(),
});

export const SandboxEvent = z.discriminatedUnion("type", [
  ConfigApplyEvent,
  EgressRequestEvent,
  EgressResponseEvent,
  EgressChunkEvent,
  FSRequestEvent,
  FSResponseEvent,
  StdioEvent,
  ResourceUsageEvent,
  ExecRequestEvent,
  ExecResponseEvent,
  IngressRequestEvent,
  IngressResponseEvent,
]);
export type SandboxEvent = z.infer<typeof SandboxEvent>;
