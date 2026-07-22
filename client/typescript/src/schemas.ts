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
 * - `s3`       — backed by Amazon S3 or an S3-compatible service.
 * - `azure`    — backed by Azure Blob Storage.
 * - `onedrive` — backed by Microsoft OneDrive.
 * - `external` — backed by an HTTP host you implement.
 */
export type Backend = "local" | "gdrive" | "gcs" | "s3" | "azure" | "onedrive" | "external";
export const Backend = z.enum(["local", "gdrive", "gcs", "s3", "azure", "onedrive", "external"]);
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

/** A file system backed by Amazon S3 or an S3-compatible service. */
export interface S3FileSystem extends FileSystemBase {
  backend: "s3";
  /** S3 bucket name. */
  s3_bucket: string;
  /** AWS region of the bucket (e.g. `us-east-1`). Required for AWS; some S3-compatible services accept `auto`. */
  s3_region?: string;
  /** Optional key prefix within the bucket (e.g. `workspace/session-42`). When omitted, the bucket root is used. */
  s3_prefix?: string;
  /** Access key ID for the S3 credentials. */
  s3_access_key_id: string;
  /** Secret access key for the S3 credentials. */
  s3_secret_access_key: string;
  /** Optional session token, for temporary (STS) credentials. */
  s3_session_token?: string;
  /** Optional custom endpoint URL for S3-compatible services (MinIO, Cloudflare R2, Backblaze B2). */
  s3_endpoint?: string;
  /** Use path-style addressing instead of virtual-hosted. Most S3-compatible services require this. */
  s3_use_path_style?: boolean;
}
export const S3FileSystem = FileSystemBase.extend({
  backend: z.literal("s3"),
  s3_bucket: z.string(),
  s3_region: z.string().optional(),
  s3_prefix: z.string().optional(),
  s3_access_key_id: z.string(),
  s3_secret_access_key: z.string(),
  s3_session_token: z.string().optional(),
  s3_endpoint: z.string().optional(),
  s3_use_path_style: z.boolean().optional(),
});
type _AssertS3FileSystem = Expect<
  Equal<z.infer<typeof S3FileSystem>, S3FileSystem>
>;

/** A file system backed by Azure Blob Storage. */
export interface AzureBlobFileSystem extends FileSystemBase {
  backend: "azure";
  /** Storage account name. Required unless `azure_connection_string` or `azure_endpoint` is set. */
  azure_account?: string;
  /** Blob container name (the Azure equivalent of a bucket). */
  azure_container: string;
  /** Optional key prefix within the container (e.g. `workspace/session-42`). When omitted, the container root is used. */
  azure_prefix?: string;
  /** Storage account access key (shared-key auth). One of key / connection string / SAS token is required. */
  azure_account_key?: string;
  /** Full connection string (account, key, endpoint). Takes precedence over the other credential fields. */
  azure_connection_string?: string;
  /** Shared access signature token authorizing the container. A leading `?` is optional. */
  azure_sas_token?: string;
  /** Optional custom blob service endpoint (e.g. the Azurite emulator). Defaults to `https://{account}.blob.core.windows.net`. */
  azure_endpoint?: string;
}
export const AzureBlobFileSystem = FileSystemBase.extend({
  backend: z.literal("azure"),
  azure_account: z.string().optional(),
  azure_container: z.string(),
  azure_prefix: z.string().optional(),
  azure_account_key: z.string().optional(),
  azure_connection_string: z.string().optional(),
  azure_sas_token: z.string().optional(),
  azure_endpoint: z.string().optional(),
});
type _AssertAzureBlobFileSystem = Expect<
  Equal<z.infer<typeof AzureBlobFileSystem>, AzureBlobFileSystem>
>;

/** A file system backed by Microsoft OneDrive (via the Microsoft Graph API). */
export interface OneDriveFileSystem extends FileSystemBase {
  backend: "onedrive";
  /** OAuth access token. */
  onedrive_access_token: string;
  /** OAuth refresh token; pair with client id/secret to enable refresh. */
  onedrive_refresh_token?: string;
  /** OAuth application (client) ID. */
  onedrive_client_id?: string;
  /** OAuth client secret. */
  onedrive_client_secret?: string;
  /** Microsoft identity platform tenant used for token refresh. Defaults to `common`. */
  onedrive_tenant?: string;
  /** Target a specific drive (e.g. a SharePoint document library). Defaults to the user's OneDrive. */
  onedrive_drive_id?: string;
  /** Optional subfolder path the file system is scoped to (e.g. `e2e-test/run-42`). Created if absent. */
  onedrive_prefix?: string;
}
export const OneDriveFileSystem = FileSystemBase.extend({
  backend: z.literal("onedrive"),
  onedrive_access_token: z.string(),
  onedrive_refresh_token: z.string().optional(),
  onedrive_client_id: z.string().optional(),
  onedrive_client_secret: z.string().optional(),
  onedrive_tenant: z.string().optional(),
  onedrive_drive_id: z.string().optional(),
  onedrive_prefix: z.string().optional(),
});
type _AssertOneDriveFileSystem = Expect<
  Equal<z.infer<typeof OneDriveFileSystem>, OneDriveFileSystem>
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
  | S3FileSystem
  | AzureBlobFileSystem
  | OneDriveFileSystem
  | ExternalFileSystem;
export const FileSystem = z.discriminatedUnion("backend", [
  LocalFileSystem,
  GDriveFileSystem,
  GCSFileSystem,
  S3FileSystem,
  AzureBlobFileSystem,
  OneDriveFileSystem,
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
  /**
   * Request body the proxy sends upstream in place of the agent's. A string always replaces the
   * body verbatim. An object is applied per `body_strategy` (default `merge`): `merge` shallow-merges
   * it into the agent's JSON body (top-level keys here overwrite the agent's, all other keys are
   * preserved — agent `{a:1,b:2}` with `{b:3}` sends `{a:1,b:3}`; if the agent's body is absent or
   * not a JSON object the override object is sent as-is), while `replace` discards the agent's body
   * and sends the override object as-is. Applies to inspected HTTP requests only; CONNECT and
   * passthrough TLS are unaffected.
   */
  body?: string | Record<string, unknown>;
  /**
   * How an object `body` override is applied. `merge` (the default) shallow-merges the override into
   * the agent's JSON body; `replace` discards the agent's body and sends the override object as-is.
   * Ignored when `body` is a string (a string always replaces the body verbatim).
   */
  body_strategy?: "merge" | "replace";
}
export const EgressOverride = z.object({
  host: z.string().optional(),
  prefix_path: z.string().optional(),
  query: z.record(z.string(), z.string()).optional(),
  headers: z.record(z.string(), z.string()).optional(),
  body: z.union([z.string(), z.record(z.string(), z.unknown())]).optional(),
  body_strategy: z.enum(["merge", "replace"]).optional(),
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
  /**
   * Git-style glob path patterns matched by this rule. Empty means any path. Matching is
   * segment-by-segment on `/` boundaries. `*` matches zero or more characters within a single segment
   * and never crosses a slash (`/users/*` matches `/users/42` and `/users/` but not `/users/42/posts`;
   * `/files/log-*` matches `/files/log-2024`). `**` matches zero or more whole segments (`/repos/**`
   * matches `/repos`, `/repos/foo`, and `/repos/foo/bar`), and may appear mid-path to span any depth
   * between two literal segments.
   */
  paths?: string[];
  override?: EgressOverride;
  /**
   * Optional Lua script run against matching inspected HTTP requests, after `override` is applied.
   * It can rewrite the request body and headers programmatically. Runs in a restricted VM
   * (base/string/table/math only) with globals `body` (string), `headers` (name→value table), and
   * read-only `method`/`host`/`path`/`query`; helpers `urldecode`/`urlencode`/`b64decode`/`b64encode`.
   * Inspected HTTP only.
   */
  override_script?: string;
}
export const EgressRule = z.object({
  access: z.enum(["allow", "deny"]),
  host: z.string(),
  ports: z.array(z.number().int()).optional(),
  methods: z.array(HttpMethod).optional(),
  paths: z.array(z.string()).optional(),
  override: EgressOverride.optional(),
  override_script: z.string().optional(),
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
   *
   * Snapshot/resume: the guest vCPU count is a boot-time property baked into a
   * VM snapshot at capture, so a sandbox resumed from a snapshot
   * (snapshot.vm.key) gets the vCPUs the guest had when the snapshot was
   * CREATED — set `cpu` when creating the snapshot to size the guest. On resume
   * `cpu` still applies the VMM's cgroup CPU quota (unthrottling the guest's
   * existing vCPUs) but cannot add vCPUs to an already-captured guest. So a warm
   * workload captured once and resumed per request — e.g. a resident browser —
   * should set `cpu` both when creating the snapshot and on resume.
   */
  cpu?: number;
  /**
   * Memory allocated to the sandbox, in MiB (microvm: guest RAM size;
   * container: enforced as a cgroup memory limit). Defaults to 512.
   * This cannot be changed after the sandbox is initialized.
   *
   * Snapshot/resume: like `cpu`, guest RAM is a boot-time property baked into a
   * VM snapshot at capture; a resumed sandbox gets the RAM the guest had when
   * the snapshot was CREATED. Set `memory` when creating the snapshot to size
   * guest RAM; on resume it applies the cgroup memory limit but does not resize
   * an already-captured guest. As with `cpu`, set it both when creating the
   * snapshot and on resume (e.g. for a resident browser).
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
const SandboxConfigObject = z.object({
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

// Go marshals nil slices/maps/pointers as JSON `null` rather than omitting them,
// so a stored config round-trips back with `fs: null`, `egress: null`, etc. The
// fields above are `.optional()` (accept `undefined`, not `null`), which would
// make `getConfig()` — and re-applying a read-back config — throw. Strip
// null-valued top-level keys so they're treated as unset on both read and write.
export const SandboxConfig = z.preprocess((val) => {
  if (val && typeof val === "object" && !Array.isArray(val)) {
    return Object.fromEntries(
      Object.entries(val as Record<string, unknown>).filter(
        ([, v]) => v !== null,
      ),
    );
  }
  return val;
}, SandboxConfigObject);
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
  operation: "read" | "write" | "delete";
}
export const FSRequestEvent = SandboxEventBase.extend({
  type: z.literal("fs.request"),
  access: z.enum(["allowed", "denied"]),
  mount: z.string(),
  path: z.string(),
  operation: z.enum(["read", "write", "delete"]),
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
}
export const IngressResponseEvent = SandboxEventBase.extend({
  type: z.literal("ingress.response"),
  request_id: z.number().int(),
  status: z.number().int(),
  duration_ms: z.number().int(),
  headers: z.record(z.string(), z.string()).optional(),
});
type _AssertIngressResponseEvent = Expect<
  Equal<z.infer<typeof IngressResponseEvent>, IngressResponseEvent>
>;

export interface IngressChunkEvent extends SandboxEventBase {
  type: "ingress.chunk";
  request_id: number;
  body: string;
  /** Optional origin tag: `up` for caller→sandbox, `down` for sandbox→caller (WebSocket only). */
  label?: string;
}
export const IngressChunkEvent = SandboxEventBase.extend({
  type: z.literal("ingress.chunk"),
  request_id: z.number(),
  body: z.string(),
  label: z.string().optional(),
});
type _AssertIngressChunkEvent = Expect<
  Equal<z.infer<typeof IngressChunkEvent>, IngressChunkEvent>
>;

export interface SystemEvent extends SandboxEventBase {
  type:
    | "system.start"
    | "system.config-changed"
    | "system.vm-resumed"
    | "system.shutdown";
  config?: SandboxConfig;
}
export const SystemEvent = SandboxEventBase.extend({
  type: z.enum([
    "system.start",
    "system.config-changed",
    "system.vm-resumed",
    "system.shutdown",
  ]),
  config: SandboxConfig.optional(),
});
type _AssertSystemEvent = Expect<
  Equal<z.infer<typeof SystemEvent>, SystemEvent>
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
  | IngressResponseEvent
  | IngressChunkEvent
  | SystemEvent;
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
  IngressChunkEvent,
  SystemEvent,
]);
type _AssertSandboxEvent = Expect<
  Equal<z.infer<typeof SandboxEvent>, SandboxEvent>
>;
