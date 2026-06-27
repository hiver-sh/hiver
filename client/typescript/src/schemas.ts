import { z } from "zod";
import type { Sandbox } from "./sandbox";

/**
 * Compile-time guard: fails to typecheck if the hand-written public type drifts
 * from the schema's inferred output. Docs live on the interfaces/types below
 * (so editors show them on hover); these assertions keep the shapes honest.
 */
type Equal<A, B> =
  (<T>() => T extends A ? 1 : 2) extends <T>() => T extends B ? 1 : 2
    ? true
    : false;
type Expect<T extends true> = T;

/**
 * Storage type for a file system.
 * - `local`    — sandbox-local storage with no external dependency.
 * - `gdrive`   — backed by Google Drive.
 * - `gcs`      — backed by Google Cloud Storage.
 * - `external` — backed by an HTTP host you implement.
 */
export type Backend = "local" | "gdrive" | "gcs" | "external";
export const Backend = z.enum(["local", "gdrive", "gcs", "external"]);
type _AssertBackend = Expect<Equal<z.infer<typeof Backend>, Backend>>;

/** One access control rule. */
export interface ACLRule {
  /** Path or glob the rule applies to (e.g. `/workspace/secret/**`). Rules are matched longest-prefix-first; access is denied by default when no rule matches. */
  path: string;
  /** Read-write, read-only, or fully denied. */
  access: "rw" | "ro" | "deny";
}
export const ACLRule = z.object({
  path: z.string(),
  access: z.enum(["rw", "ro", "deny"]),
});
type _AssertACLRule = Expect<Equal<z.infer<typeof ACLRule>, ACLRule>>;

/** Fields shared by every file system variant. */
export interface FileSystemBase {
  /** Absolute path at which the file system appears to the agent. */
  mount: string;
  /** Access control rules for paths under `mount`. */
  acls?: ACLRule[];
  /**
   * When true, the file system is mounted inside the sandbox runtime but
   * hidden from the agent workload. Use it for storage the sandbox needs but
   * the agent must not see, e.g. a remote-backed snapshot target referenced by
   * `Snapshot.mount`. Because the agent cannot reach the mount, `acls` are
   * ignored for internal file systems.
   */
  internal?: boolean;
}
const FileSystemBase = z.object({
  mount: z.string().regex(/^\/.+/, "mount must be an absolute path"),
  acls: z.array(ACLRule).optional(),
  internal: z.boolean().optional(),
});

/** Sandbox-local storage with no external dependency. */
export interface LocalFileSystem extends FileSystemBase {
  backend: "local";
  /**
   * The local path to mount into this sandbox.
   * Local origins are only supported locally with the Docker runtime.
   * Helpful for local development, e.g. mounting local skill files into the sandbox.
   */
  origin?: string;
}
export const LocalFileSystem = FileSystemBase.extend({
  backend: z.literal("local"),
  origin: z.string().optional(),
});
type _AssertLocalFileSystem = Expect<
  Equal<z.infer<typeof LocalFileSystem>, LocalFileSystem>
>;

/** A file system backed by Google Drive. */
export interface GDriveFileSystem extends FileSystemBase {
  backend: "gdrive";
  /** OAuth access token. */
  gdrive_access_token?: string;
  /** OAuth refresh token. */
  gdrive_refresh_token?: string;
  /** OAuth client ID. */
  gdrive_client_id?: string;
  /** OAuth client secret. */
  gdrive_client_secret?: string;
  /** Service account credential JSON. Mutually exclusive with the OAuth fields above. */
  gdrive_service_account_json?: string;
  /** ID of the Drive folder the file system is scoped to. When omitted, the account root is used. */
  gdrive_folder_id?: string;
  /** Optional subfolder path within gdrive_folder_id (e.g. `e2e-test/run-42`). Created if absent. */
  gdrive_prefix?: string;
}
export const GDriveFileSystem = FileSystemBase.extend({
  backend: z.literal("gdrive"),
  gdrive_access_token: z.string().optional(),
  gdrive_refresh_token: z.string().optional(),
  gdrive_client_id: z.string().optional(),
  gdrive_client_secret: z.string().optional(),
  gdrive_service_account_json: z.string().optional(),
  gdrive_folder_id: z.string().optional(),
  gdrive_prefix: z.string().optional(),
});
type _AssertGDriveFileSystem = Expect<
  Equal<z.infer<typeof GDriveFileSystem>, GDriveFileSystem>
>;

/** A file system backed by Google Cloud Storage. */
export interface GCSFileSystem extends FileSystemBase {
  backend: "gcs";
  /** GCS bucket name. */
  gcs_bucket: string;
  /** Optional key prefix within the bucket (e.g. `workspace/session-42`). When omitted, the bucket root is used. */
  gcs_prefix?: string;
  /**
   * Service account credential JSON. When omitted, Application Default Credentials are used
   * (GOOGLE_APPLICATION_CREDENTIALS env var, gcloud user credentials, or the GCE/GKE metadata server).
   */
  gcs_service_account_json: string;
}
export const GCSFileSystem = FileSystemBase.extend({
  backend: z.literal("gcs"),
  gcs_bucket: z.string(),
  gcs_prefix: z.string().optional(),
  gcs_service_account_json: z.string(),
});
type _AssertGCSFileSystem = Expect<
  Equal<z.infer<typeof GCSFileSystem>, GCSFileSystem>
>;

/**
 * A file system backed by an external HTTP host you implement. Each agent file
 * operation becomes one call against `host`.
 */
export interface ExternalFileSystem extends FileSystemBase {
  backend: "external";
  /**
   * Base URL of the host implementing the external file system interface.
   * A trailing slash is ignored.
   */
  host: string;
}
export const ExternalFileSystem = FileSystemBase.extend({
  backend: z.literal("external"),
  host: z.string(),
});
type _AssertExternalFileSystem = Expect<
  Equal<z.infer<typeof ExternalFileSystem>, ExternalFileSystem>
>;

/**
 * A file system exposed to the agent at `mount`. The `backend` selects the storage type and
 * determines which variant applies. Access is governed by `acls`, evaluated longest-prefix-first
 * with deny as the default.
 */
export type FileSystem =
  | LocalFileSystem
  | GDriveFileSystem
  | GCSFileSystem
  | ExternalFileSystem;
export const FileSystem = z.discriminatedUnion("backend", [
  LocalFileSystem,
  GDriveFileSystem,
  GCSFileSystem,
  ExternalFileSystem,
]);
type _AssertFileSystem = Expect<Equal<z.infer<typeof FileSystem>, FileSystem>>;

export type HttpMethod =
  | "GET"
  | "POST"
  | "PUT"
  | "PATCH"
  | "DELETE"
  | "HEAD"
  | "OPTIONS";
export const HttpMethod = z.enum([
  "GET",
  "POST",
  "PUT",
  "PATCH",
  "DELETE",
  "HEAD",
  "OPTIONS",
]);
type _AssertHttpMethod = Expect<Equal<z.infer<typeof HttpMethod>, HttpMethod>>;

/**
 * Values the proxy injects into outbound requests that match an egress rule.
 * If the agent already set the same query parameter or header, the proxy overwrites it;
 * otherwise the value is added. The agent cannot read these values back.
 */
export interface EgressOverride {
  /**
   * Upstream the proxy dials instead of the matched host, as `hostname[:port]` or `ip[:port]`.
   * When the port is omitted, the original destination port is kept. Rule matching and the
   * agent-visible request (Host header, TLS SNI) keep the original hostname; audit events report
   * the substituted dial target in `upstream`.
   */
  host?: string;
  /**
   * Path prefix prepended to the outbound request path (`/mock` turns `/v1/user` into `/mock/v1/user`).
   * Rule matching and audit events keep the agent's original path. Applies to inspected HTTP requests
   * only; CONNECT and passthrough TLS are unaffected. A trailing slash is ignored.
   */
  prefix_path?: string;
  /** URL query parameters to add or overwrite on the outbound request. Useful for injecting API keys the agent should never see. */
  query?: Record<string, string>;
  /** HTTP headers to add or overwrite on the outbound request. Useful for injecting bearer tokens or tenant identifiers. */
  headers?: Record<string, string>;
}
export const EgressOverride = z.object({
  host: z.string().optional(),
  prefix_path: z.string().optional(),
  query: z.record(z.string(), z.string()).optional(),
  headers: z.record(z.string(), z.string()).optional(),
});
type _AssertEgressOverride = Expect<
  Equal<z.infer<typeof EgressOverride>, EgressOverride>
>;

/** One egress rule. */
export interface EgressRule {
  /** Whether matching requests are allowed or denied. */
  access: "allow" | "deny";
  /** Exact host (`api.github.com`) or wildcard suffix (`*.pypi.org`). */
  host: string;
  /** Optional ports; when omitted no port enforcement is performed. */
  ports?: number[];
  /** HTTP methods matched by this rule. Empty means any method. */
  methods?: HttpMethod[];
  /** Glob path patterns matched by this rule. Empty means any path. */
  paths?: string[];
  override?: EgressOverride;
}
export const EgressRule = z.object({
  access: z.enum(["allow", "deny"]),
  host: z.string(),
  ports: z.array(z.number().int()).optional(),
  methods: z.array(HttpMethod).optional(),
  paths: z.array(z.string()).optional(),
  override: EgressOverride.optional(),
});
type _AssertEgressRule = Expect<Equal<z.infer<typeof EgressRule>, EgressRule>>;

/** microVM-state snapshot. When a VM snapshot exists under `key`, a get-or-create
 * resumes it instead of cold-booting; otherwise the VM cold-boots and the client
 * captures the snapshot explicitly. Ignored by the container backend. */
export interface SnapshotVM {
  /** Key identifying the VM-state snapshot. */
  key: string;
}
export const SnapshotVM = z.object({
  key: z.string().regex(/^[A-Za-z0-9_-]{1,64}$/),
});
type _AssertSnapshotVM = Expect<Equal<z.infer<typeof SnapshotVM>, SnapshotVM>>;

/** Writable-filesystem snapshot, captured as a portable gzip-tar. Restored when
 * the sandbox starts and written by the snapshot action (or on shutdown when
 * `write_on_shutdown` is set). */
export interface SnapshotFiles {
  /** Key identifying the files snapshot. */
  key: string;
  /** When true, the files snapshot is captured on shutdown or termination. When
   * false (the default), files are captured only by an explicit snapshot action. */
  write_on_shutdown?: boolean;
  /** Glob patterns specifying which paths to include in the snapshot (e.g. `/home/user/*`). */
  include?: string[];
  /**
   * Mount path of a file system (see `SandboxConfig.fs`) where the files tarball
   * is written and read, instead of the host's local snapshot directory. Point it
   * at an `internal`, remote-backed file system to persist and restore through a
   * FUSE drive.
   */
  mount?: string;
}
export const SnapshotFiles = z.object({
  key: z.string().regex(/^[A-Za-z0-9_-]{1,64}$/),
  write_on_shutdown: z.boolean().optional(),
  include: z.array(z.string()).min(1).optional(),
  mount: z.string().regex(/^\/.+/).optional(),
});
type _AssertSnapshotFiles = Expect<Equal<z.infer<typeof SnapshotFiles>, SnapshotFiles>>;

/**
 * Snapshot configuration. It has two independent parts: `vm` captures the full
 * microVM state (a no-op for the container backend) and `files` captures the
 * writable filesystem as a portable tarball. Either may be present alone, both,
 * or neither.
 */
export interface Snapshot {
  /** microVM-state snapshot (a no-op for the container backend). */
  vm?: SnapshotVM;
  /** Writable-filesystem snapshot. */
  files?: SnapshotFiles;
}
export const Snapshot = z.object({
  vm: SnapshotVM.optional(),
  files: SnapshotFiles.optional(),
});
type _AssertSnapshot = Expect<Equal<z.infer<typeof Snapshot>, Snapshot>>;

/** Outcome of capturing one snapshot part. */
export interface SnapshotPartResult {
  /** Whether this part was captured. False (with `reason`) when unsupported on the active backend. */
  captured: boolean;
  /** Key the part was written under. */
  key: string;
  /** Size of the captured artifact in bytes, when known. */
  bytes?: number;
  /** Why the part was not captured, when `captured` is false. */
  reason?: string;
}
export const SnapshotPartResult = z.object({
  captured: z.boolean(),
  key: z.string(),
  bytes: z.number().optional(),
  reason: z.string().optional(),
});
type _AssertSnapshotPartResult = Expect<Equal<z.infer<typeof SnapshotPartResult>, SnapshotPartResult>>;

/** Outcome of a snapshot action, reported independently per requested part. */
export interface SnapshotResult {
  vm?: SnapshotPartResult;
  files?: SnapshotPartResult;
}
export const SnapshotResult = z.object({
  vm: SnapshotPartResult.optional(),
  files: SnapshotPartResult.optional(),
});
type _AssertSnapshotResult = Expect<Equal<z.infer<typeof SnapshotResult>, SnapshotResult>>;

/** Hive sandbox configuration. */
export interface SandboxConfig {
  /** Reference to the agent image to launch. This cannot be changed after the sandbox is initialized. */
  image?: string;
  /**
   * Number of virtual CPUs allocated to the sandbox, as a ceiling (the pod CPU
   * limit). May be fractional (e.g. 0.5); the microvm guest vCPU count is this
   * value rounded up, the container enforces it as a CPU quota. Defaults to 1.
   * This cannot be changed after the sandbox is initialized.
   */
  cpu?: number;
  /**
   * Memory allocated to the sandbox, in MiB (microvm: guest RAM size;
   * container: enforced as a cgroup memory limit). Defaults to 512.
   * This cannot be changed after the sandbox is initialized.
   */
  memory?: number;
  /** Override the entrypoint used when the container is run. Accepts either an argv array (each element a separate argument) or a single string, which the sandbox splits on whitespace into arguments. When omitted, the image's default entrypoint is used. */
  entrypoint?: string | string[];
  /**
   * Working directory for the entrypoint. When set, the entrypoint is launched
   * with this as its current working directory, overriding the image's default
   * working directory. When omitted, the image's working directory is used.
   * This cannot be changed after the sandbox is initialized.
   */
  cwd?: string;
  /**
   * When true, the entrypoint is launched attached to a pseudo-TTY. A client can
   * then attach to that terminal by calling `execStream` with an empty command.
   * Only supported for the `container` isolation. Defaults to false.
   * This cannot be changed after the sandbox is initialized.
   */
  tty?: boolean;
  /** Additional environment variables in `KEY=VALUE` form. This cannot be changed after the sandbox is initialized. */
  env?: Record<string, string>;
  /** Additional /etc/hosts entries in `hostname:ip` form (use `host-gateway` for the host machine's IP). Cannot be changed after the sandbox is initialized. */
  extra_hosts?: string[];
  /**
   * Sandbox time to live in seconds. Call {@link Sandbox.ping} to reset the
   * timer; once a ping has not been received for this long the sandbox is
   * stopped. Defaults to 1800 (30 min). Use `0` to disable shutdown.
   */
  ttl?: number;
  /**
   * One or more file systems exposed to the agent. Mount paths must be unique and
   * non-overlapping (a mount path may not be a parent directory of another mount path).
   */
  fs?: FileSystem[];
  /**
   * Ordered list of egress rules. The first rule that matches a request decides the outcome;
   * requests that match no rule are denied.
   */
  egress?: EgressRule[];
  snapshot?: Snapshot;
}
export const SandboxConfig = z.object({
  image: z.string().optional(),
  cpu: z.number().positive().optional(),
  memory: z.number().int().min(128).optional(),
  entrypoint: z.union([z.string(), z.array(z.string())]).optional(),
  cwd: z.string().optional(),
  tty: z.boolean().optional(),
  env: z.record(z.string(), z.string()).optional(),
  extra_hosts: z.array(z.string()).optional(),
  ttl: z.number().int().min(0).optional(),
  fs: z.array(FileSystem).min(1).optional(),
  egress: z.array(EgressRule).optional(),
  snapshot: Snapshot.optional(),
});
type _AssertSandboxConfig = Expect<
  Equal<z.infer<typeof SandboxConfig>, SandboxConfig>
>;

/** Internal runtime information about a sandbox, determined at boot rather than configured. */
export interface SandboxInfo {
  /**
   * The isolation mechanism the sandbox is running with. Selected automatically
   * from the image — a microvm image ships a guest root filesystem — not from config.
   */
  isolation: "container" | "microvm";
}
export const SandboxInfo = z.object({
  isolation: z.enum(["container", "microvm"]),
});
type _AssertSandboxInfo = Expect<
  Equal<z.infer<typeof SandboxInfo>, SandboxInfo>
>;

/**
 * Concrete additions and removals carried out by an apply call. Each list contains whole entries
 * (a complete `FileSystem` or `EgressRule`) so the caller can audit what changed without
 * re-diffing the request.
 */
export interface Changes {
  fs?: {
    added?: FileSystem[];
    removed?: FileSystem[];
  };
  egress?: {
    added?: EgressRule[];
    removed?: EgressRule[];
  };
  /** Non-fatal advisories. Example: a non-modifiable field was present in the request and was ignored. */
  warnings?: string[];
}
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
  warnings: z.array(z.string()).optional(),
});
type _AssertChanges = Expect<Equal<z.infer<typeof Changes>, Changes>>;

/** The outcome of an apply call. */
export interface ApplyResult {
  /** `true` if every change was applied successfully. `false` if the apply failed and was rolled back; in that case the sandbox is unchanged. */
  applied: boolean;
  /**
   * The configuration in effect after this call. When `applied` is `true` this matches the
   * request, except for any non-modifiable fields preserved from the previous state
   * (see `changes.warnings`).
   */
  config: SandboxConfig;
  changes: Changes;
  /** Human-readable failure reason. Set only when `applied` is `false`. */
  error?: string;
}
export const ApplyResult = z.object({
  applied: z.boolean(),
  config: SandboxConfig,
  changes: Changes,
  error: z.string().optional(),
});
type _AssertApplyResult = Expect<
  Equal<z.infer<typeof ApplyResult>, ApplyResult>
>;

export interface ApiError {
  /** Human-readable failure reason. */
  error: string;
  /** Optional structured context such as the offending field path or a conflict identifier. */
  details?: Record<string, unknown>;
}
export const ApiError = z.object({
  error: z.string(),
  details: z.record(z.string(), z.unknown()).optional(),
});
type _AssertApiError = Expect<Equal<z.infer<typeof ApiError>, ApiError>>;

/** Controller response: a provisioned sandbox handle. */
export interface SandboxRef {
  /** Server-assigned unique identifier (uuid). */
  id: string;
  /** Caller-chosen key the sandbox was provisioned under; used for routing. */
  key: string;
}
export const SandboxRef = z.object({
  id: z.string(),
  key: z.string(),
});
type _AssertSandboxRef = Expect<Equal<z.infer<typeof SandboxRef>, SandboxRef>>;

/** Fields shared by every sandbox event. */
export interface SandboxEventBase {
  id: number;
  timestamp: string;
}
const SandboxEventBase = z.object({
  id: z.number().int(),
  timestamp: z.string(),
});

export interface ConfigApplyEvent extends SandboxEventBase {
  type: "config.apply";
  success: boolean;
  changes: Changes;
  errorMessage?: string;
}
export const ConfigApplyEvent = SandboxEventBase.extend({
  type: z.literal("config.apply"),
  success: z.boolean(),
  changes: Changes,
  errorMessage: z.string().optional(),
});
type _AssertConfigApplyEvent = Expect<
  Equal<z.infer<typeof ConfigApplyEvent>, ConfigApplyEvent>
>;

export interface EgressRequestEvent extends SandboxEventBase {
  type: "egress.request";
  access: "allowed" | "denied";
  host: string;
  method: string;
  path: string;
  query?: string;
  headers?: Record<string, string>;
  body?: string;
}
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
type _AssertEgressRequestEvent = Expect<
  Equal<z.infer<typeof EgressRequestEvent>, EgressRequestEvent>
>;

export interface EgressResponseEvent extends SandboxEventBase {
  type: "egress.response";
  request_id: number;
  status: number;
  duration_ms: number;
  headers?: Record<string, string>;
}
export const EgressResponseEvent = SandboxEventBase.extend({
  type: z.literal("egress.response"),
  request_id: z.number(),
  status: z.number().int(),
  duration_ms: z.number().int(),
  headers: z.record(z.string(), z.string()).optional(),
});
type _AssertEgressResponseEvent = Expect<
  Equal<z.infer<typeof EgressResponseEvent>, EgressResponseEvent>
>;

export interface EgressChunkEvent extends SandboxEventBase {
  type: "egress.chunk";
  request_id: number;
  body: string;
  /** Optional origin tag: `up` for client→upstream, `down` for upstream→client (WebSocket only). */
  label?: string;
}
export const EgressChunkEvent = SandboxEventBase.extend({
  type: z.literal("egress.chunk"),
  request_id: z.number(),
  body: z.string(),
  label: z.string().optional(),
});
type _AssertEgressChunkEvent = Expect<
  Equal<z.infer<typeof EgressChunkEvent>, EgressChunkEvent>
>;

export interface FSRequestEvent extends SandboxEventBase {
  type: "fs.request";
  access: "allowed" | "denied";
  mount: string;
  path: string;
  operation: "read" | "write";
}
export const FSRequestEvent = SandboxEventBase.extend({
  type: z.literal("fs.request"),
  access: z.enum(["allowed", "denied"]),
  mount: z.string(),
  path: z.string(),
  operation: z.enum(["read", "write"]),
});
type _AssertFSRequestEvent = Expect<
  Equal<z.infer<typeof FSRequestEvent>, FSRequestEvent>
>;

export interface FSResponseEvent extends SandboxEventBase {
  type: "fs.response";
  backend: Backend;
  request_id: number;
  duration_ms: number;
  error?: string;
}
export const FSResponseEvent = SandboxEventBase.extend({
  type: z.literal("fs.response"),
  backend: Backend,
  request_id: z.number(),
  duration_ms: z.number().int(),
  error: z.string().optional(),
});
type _AssertFSResponseEvent = Expect<
  Equal<z.infer<typeof FSResponseEvent>, FSResponseEvent>
>;

export interface StdioEvent extends SandboxEventBase {
  type: "stdio";
  stdout?: string;
  stderr?: string;
}
export const StdioEvent = SandboxEventBase.extend({
  type: z.literal("stdio"),
  stdout: z.string().optional(),
  stderr: z.string().optional(),
});
type _AssertStdioEvent = Expect<Equal<z.infer<typeof StdioEvent>, StdioEvent>>;

export interface ResourceUsageEvent extends SandboxEventBase {
  type: "resource.usage";
  cpu_percent: number;
  memory_bytes: number;
}
export const ResourceUsageEvent = SandboxEventBase.extend({
  type: z.literal("resource.usage"),
  cpu_percent: z.number(),
  memory_bytes: z.number().int(),
});
type _AssertResourceUsageEvent = Expect<
  Equal<z.infer<typeof ResourceUsageEvent>, ResourceUsageEvent>
>;

export interface ExecRequestEvent extends SandboxEventBase {
  type: "exec.request";
  cwd: string;
  command: string;
}
export const ExecRequestEvent = SandboxEventBase.extend({
  type: z.literal("exec.request"),
  cwd: z.string(),
  command: z.string(),
});
type _AssertExecRequestEvent = Expect<
  Equal<z.infer<typeof ExecRequestEvent>, ExecRequestEvent>
>;

export interface ExecResponseEvent extends SandboxEventBase {
  type: "exec.response";
  request_id: number;
}
export const ExecResponseEvent = SandboxEventBase.extend({
  type: z.literal("exec.response"),
  request_id: z.number().int(),
});
type _AssertExecResponseEvent = Expect<
  Equal<z.infer<typeof ExecResponseEvent>, ExecResponseEvent>
>;

export interface IngressRequestEvent extends SandboxEventBase {
  type: "ingress.request";
  port: string;
  method: string;
  path: string;
  query?: string;
  headers?: Record<string, string>;
  body?: string;
}
export const IngressRequestEvent = SandboxEventBase.extend({
  type: z.literal("ingress.request"),
  port: z.string(),
  method: z.string(),
  path: z.string(),
  query: z.string().optional(),
  headers: z.record(z.string(), z.string()).optional(),
  body: z.string().optional(),
});
type _AssertIngressRequestEvent = Expect<
  Equal<z.infer<typeof IngressRequestEvent>, IngressRequestEvent>
>;

export interface IngressResponseEvent extends SandboxEventBase {
  type: "ingress.response";
  request_id: number;
  status: number;
  duration_ms: number;
  headers?: Record<string, string>;
  body?: string;
}
export const IngressResponseEvent = SandboxEventBase.extend({
  type: z.literal("ingress.response"),
  request_id: z.number().int(),
  status: z.number().int(),
  duration_ms: z.number().int(),
  headers: z.record(z.string(), z.string()).optional(),
  body: z.string().optional(),
});
type _AssertIngressResponseEvent = Expect<
  Equal<z.infer<typeof IngressResponseEvent>, IngressResponseEvent>
>;

export type SandboxEvent =
  | ConfigApplyEvent
  | EgressRequestEvent
  | EgressResponseEvent
  | EgressChunkEvent
  | FSRequestEvent
  | FSResponseEvent
  | StdioEvent
  | ResourceUsageEvent
  | ExecRequestEvent
  | ExecResponseEvent
  | IngressRequestEvent
  | IngressResponseEvent;
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
type _AssertSandboxEvent = Expect<
  Equal<z.infer<typeof SandboxEvent>, SandboxEvent>
>;
