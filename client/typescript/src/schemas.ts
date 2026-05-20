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

/** One egress allow rule. */
export const EgressRule = z.object({
  /** Exact host (`api.github.com`) or wildcard suffix (`*.pypi.org`). */
  host: z.string(),
  /** Optional ports; when omitted no port enforcement is performed. */
  ports: z.array(z.number().int()).optional(),
  /** HTTP methods allowed by this rule. Empty means any method. */
  methods: z.array(HttpMethod).optional(),
  /** Glob path patterns allowed by this rule. Empty means any path. */
  paths: z.array(z.string()).optional(),
  override: EgressOverride.optional(),
});
export type EgressRule = z.infer<typeof EgressRule>;

/** Network egress configuration. */
export const Egress = z.object({
  /**
   * Ordered list of allow rules. The first rule that matches a request decides the outcome;
   * requests that match no rule are denied.
   */
  allow: z.array(EgressRule).optional(),
});
export type Egress = z.infer<typeof Egress>;

/** Hive sandbox configuration. */
export const SandboxConfig = z.object({
  /** Reference to the agent image to launch. This cannot be changed after the sandbox is initialized. */
  image: z.string().optional(),
  /** Additional environment variables in `KEY=VALUE` form. This cannot be changed after the sandbox is initialized. */
  env: z.record(z.string(), z.string()).optional(),
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
  fs: z.array(FileSystem).min(1),
  /** Network egress configuration. Controls which outbound hosts and paths the agent may reach. */
  egress: Egress.optional(),
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
  id: z.string(),
  endpoint: z.string().url(),
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
  body: z.string().optional(),
});

export const EgressResponseEvent = SandboxEventBase.extend({
  type: z.literal("egress.response"),
  request_id: z.number(),
  status: z.number().int(),
  duration_ms: z.number().int(),
  body: z.string().optional(),
});

export const EgressStreamChunkEvent = SandboxEventBase.extend({
  type: z.literal("egress.stream_chunk"),
  request_id: z.number(),
  body: z.string(),
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

export const SandboxEvent = z.discriminatedUnion("type", [
  ConfigApplyEvent,
  EgressRequestEvent,
  EgressResponseEvent,
  EgressStreamChunkEvent,
  FSRequestEvent,
  FSResponseEvent,
  StdioEvent,
]);
export type SandboxEvent = z.infer<typeof SandboxEvent>;
