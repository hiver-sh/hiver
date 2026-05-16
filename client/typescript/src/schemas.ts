import { z } from "zod";

export const Backend = z.enum(["local", "gdrive"]);
export type Backend = z.infer<typeof Backend>;

export const ACLRule = z.object({
  path: z.string(),
  access: z.enum(["rw", "ro", "deny"]),
});
export type ACLRule = z.infer<typeof ACLRule>;

const FileSystemBase = z.object({
  mount: z.string().regex(/^\/.+/, "mount must be an absolute path"),
  acls: z.array(ACLRule).optional(),
});

export const LocalFileSystem = FileSystemBase.extend({
  backend: z.literal("local"),
});
export type LocalFileSystem = z.infer<typeof LocalFileSystem>;

export const GDriveFileSystem = FileSystemBase.extend({
  backend: z.literal("gdrive"),
  gdrive_access_token: z.string().optional(),
  gdrive_refresh_token: z.string().optional(),
  gdrive_client_id: z.string().optional(),
  gdrive_client_secret: z.string().optional(),
  gdrive_service_account_json: z.string().optional(),
  gdrive_folder_id: z.string().optional(),
});
export type GDriveFileSystem = z.infer<typeof GDriveFileSystem>;

export const FileSystem = z.discriminatedUnion("backend", [
  LocalFileSystem,
  GDriveFileSystem,
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

export const EgressRule = z.object({
  host: z.string(),
  ports: z.array(z.number().int()).optional(),
  methods: z.array(HttpMethod).optional(),
  paths: z.array(z.string()).optional(),
  headers: z.record(z.string(), z.string()).optional(),
});
export type EgressRule = z.infer<typeof EgressRule>;

export const Egress = z.object({
  allow: z.array(EgressRule).optional(),
});
export type Egress = z.infer<typeof Egress>;

export const SandboxConfig = z.object({
  image: z.string().optional(),
  env: z.array(z.string()).optional(),
  ttl: z.number().int().min(0).optional(),
  fs: z.array(FileSystem).min(1),
  egress: Egress.optional(),
});
export type SandboxConfig = z.infer<typeof SandboxConfig>;

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
export type Changes = z.infer<typeof Changes>;

export const ApplyResult = z.object({
  applied: z.boolean(),
  config: SandboxConfig,
  changes: Changes,
  error: z.string().optional(),
});
export type ApplyResult = z.infer<typeof ApplyResult>;

export const ApiError = z.object({
  error: z.string(),
  details: z.record(z.string(), z.unknown()).optional(),
});
export type ApiError = z.infer<typeof ApiError>;

// Controller response: a provisioned sandbox handle.
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
});

export const EgressResponseEvent = SandboxEventBase.extend({
  type: z.literal("egress.response"),
  request_id: z.number(),
  status: z.number().int(),
  duration_ms: z.number().int(),
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
  FSRequestEvent,
  FSResponseEvent,
  StdioEvent,
]);
export type SandboxEvent = z.infer<typeof SandboxEvent>;
