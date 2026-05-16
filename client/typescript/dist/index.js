// src/schemas.ts
import { z } from "zod";
var Backend = z.enum(["local", "gdrive"]);
var ACLRule = z.object({
  path: z.string(),
  access: z.enum(["rw", "ro", "deny"])
});
var FileSystemBase = z.object({
  mount: z.string().regex(/^\/.+/, "mount must be an absolute path"),
  acls: z.array(ACLRule).optional()
});
var LocalFileSystem = FileSystemBase.extend({
  backend: z.literal("local")
});
var GDriveFileSystem = FileSystemBase.extend({
  backend: z.literal("gdrive"),
  gdrive_access_token: z.string().optional(),
  gdrive_refresh_token: z.string().optional(),
  gdrive_client_id: z.string().optional(),
  gdrive_client_secret: z.string().optional(),
  gdrive_service_account_json: z.string().optional(),
  gdrive_folder_id: z.string().optional()
});
var FileSystem = z.discriminatedUnion("backend", [
  LocalFileSystem,
  GDriveFileSystem
]);
var HttpMethod = z.enum([
  "GET",
  "POST",
  "PUT",
  "PATCH",
  "DELETE",
  "HEAD",
  "OPTIONS"
]);
var EgressRule = z.object({
  host: z.string(),
  ports: z.array(z.number().int()).optional(),
  methods: z.array(HttpMethod).optional(),
  paths: z.array(z.string()).optional(),
  headers: z.record(z.string(), z.string()).optional()
});
var Egress = z.object({
  allow: z.array(EgressRule).optional()
});
var SandboxConfig = z.object({
  image: z.string().optional(),
  env: z.array(z.string()).optional(),
  ttl: z.number().int().min(0).optional(),
  fs: z.array(FileSystem).min(1),
  egress: Egress.optional()
});
var Changes = z.object({
  fs: z.object({
    added: z.array(FileSystem).optional(),
    removed: z.array(FileSystem).optional()
  }).optional(),
  egress: z.object({
    added: z.array(EgressRule).optional(),
    removed: z.array(EgressRule).optional()
  }).optional(),
  warnings: z.array(z.string()).optional()
});
var ApplyResult = z.object({
  applied: z.boolean(),
  config: SandboxConfig,
  changes: Changes,
  error: z.string().optional()
});
var ApiError = z.object({
  error: z.string(),
  details: z.record(z.string(), z.unknown()).optional()
});
var SandboxRef = z.object({
  id: z.string(),
  endpoint: z.string().url()
});
var SandboxEventBase = z.object({
  id: z.number().int(),
  timestamp: z.string()
});
var ConfigApplyEvent = SandboxEventBase.extend({
  type: z.literal("config.apply"),
  success: z.boolean(),
  changes: Changes,
  errorMessage: z.string().optional()
});
var EgressRequestEvent = SandboxEventBase.extend({
  type: z.literal("egress.request"),
  access: z.enum(["allowed", "denied"]),
  host: z.string(),
  method: HttpMethod,
  path: z.string()
});
var EgressResponseEvent = SandboxEventBase.extend({
  type: z.literal("egress.response"),
  request_id: z.string(),
  status: z.number().int(),
  duration_ms: z.number().int()
});
var FSRequestEvent = SandboxEventBase.extend({
  type: z.literal("fs.request"),
  access: z.enum(["allowed", "denied"]),
  mount: z.string(),
  path: z.string(),
  operation: z.enum(["read", "write"])
});
var FSResponseEvent = SandboxEventBase.extend({
  type: z.literal("fs.response"),
  backend: Backend,
  request_id: z.string(),
  duration_ms: z.number().int(),
  error: z.string().optional()
});
var StdioEvent = SandboxEventBase.extend({
  type: z.literal("stdio"),
  stdout: z.string().optional(),
  stderr: z.string().optional()
});
var SandboxEvent = z.discriminatedUnion("type", [
  ConfigApplyEvent,
  EgressRequestEvent,
  EgressResponseEvent,
  FSRequestEvent,
  FSResponseEvent,
  StdioEvent
]);

// src/sse.ts
async function* parseSSE(body, signal) {
  const reader = body.getReader();
  const decoder = new TextDecoder("utf-8");
  let buffer = "";
  let lastEventId;
  const onAbort = () => {
    reader.cancel(signal?.reason).catch(() => {
    });
  };
  signal?.addEventListener("abort", onAbort, { once: true });
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      buffer = buffer.replace(/\r\n/g, "\n");
      let sep;
      while ((sep = buffer.indexOf("\n\n")) !== -1) {
        const frame = buffer.slice(0, sep);
        buffer = buffer.slice(sep + 2);
        const data = [];
        for (const raw of frame.split("\n")) {
          if (raw === "" || raw.startsWith(":")) continue;
          const colon = raw.indexOf(":");
          const field = colon === -1 ? raw : raw.slice(0, colon);
          let value2 = colon === -1 ? "" : raw.slice(colon + 1);
          if (value2.startsWith(" ")) value2 = value2.slice(1);
          if (field === "data") data.push(value2);
          else if (field === "id") lastEventId = value2;
        }
        if (data.length === 0) continue;
        yield { data: data.join("\n"), lastEventId };
      }
    }
  } finally {
    signal?.removeEventListener("abort", onAbort);
    reader.releaseLock();
  }
}

// src/sandbox.ts
var Sandbox = class {
  id;
  /** Base URL of the per-sandbox API server (no trailing slash). */
  apiServerUrl;
  /** Base URL of the controller that created this sandbox (no trailing slash). */
  controllerUrl;
  /** @internal — exposed so the controller module can share the dialer. */
  fetchImpl;
  constructor(ref, opts) {
    this.id = ref.id;
    this.apiServerUrl = ref.endpoint.replace(/\/+$/, "");
    this.controllerUrl = opts.controllerUrl.replace(/\/+$/, "");
    this.fetchImpl = opts.fetch ?? fetch;
  }
  /**
   * URL of the HTTP service the sandbox image exposes (the first TCP
   * port from its EXPOSE directive). Append paths to it to reach the
   * upstream — `${sandbox.getUrl()}/healthz`, etc.
   */
  getUrl() {
    return `${this.apiServerUrl}/v1/sandbox`;
  }
  /**
   * Reset the sandbox's TTL countdown. Bound as an arrow so
   * `setInterval(sandbox.ping, 10_000)` works without an explicit
   * `.bind(sandbox)`.
   */
  ping = async () => {
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/ping`);
    if (!res.ok) throw await toError(res, "ping");
  };
  /** Read the current `SandboxConfig`. */
  async getConfig() {
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/config`);
    if (!res.ok) throw await toError(res, "getConfig");
    return SandboxConfig.parse(await res.json());
  }
  /**
   * Apply a desired `SandboxConfig`. Returns an `ApplyResult` whose
   * `applied` field reports whether the change was committed or
   * rolled back.
   */
  async applyConfig(config) {
    const validated = SandboxConfig.parse(config);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/config`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(validated)
    });
    if (!res.ok) throw await toError(res, "applyConfig");
    return ApplyResult.parse(await res.json());
  }
  /**
   * Long-lived async iterator over `SandboxEvent`s. The HTTP request
   * is opened lazily on first `next()` and closes when the consumer
   * stops iterating or `signal` aborts.
   */
  async *getEventsStream(opts = {}) {
    const url = new URL(`${this.apiServerUrl}/v1/events`);
    if (opts.lastEventId !== void 0) {
      url.searchParams.set("lastEventId", String(opts.lastEventId));
    }
    const res = await this.fetchImpl(url, {
      headers: { accept: "text/event-stream" },
      signal: opts.signal
    });
    if (!res.ok || !res.body) throw await toError(res, "events");
    for await (const frame of parseSSE(res.body, opts.signal)) {
      yield SandboxEvent.parse(JSON.parse(frame.data));
    }
  }
  /**
   * Download a file from a sandbox mount. `path` is the agent-visible
   * absolute path (e.g. `/workspace/data.csv`). Returns the raw bytes.
   */
  async downloadFile(path) {
    const url = new URL(`${this.apiServerUrl}/v1/file`);
    url.searchParams.set("path", path);
    const res = await this.fetchImpl(url);
    if (!res.ok) throw await toError(res, "downloadFile");
    return new Uint8Array(await res.arrayBuffer());
  }
  /**
   * Upload `content` as a file to `destination` (which must equal one
   * of the configured `fs[].mount` paths). `filename` becomes the
   * basename written under `destination`. Returns the agent-visible
   * path and byte count the server reports.
   */
  async uploadFile(destination, filename, content) {
    const form = new FormData();
    form.append("destination", destination);
    form.append("file", toBlob(content), filename);
    const res = await this.fetchImpl(`${this.apiServerUrl}/v1/file`, {
      method: "POST",
      body: form
    });
    if (!res.ok) throw await toError(res, "uploadFile");
    const body = await res.json();
    return body;
  }
};
function toBlob(content) {
  if (content instanceof Blob) return content;
  if (typeof content === "string") return new Blob([content]);
  return new Blob([content]);
}
var SandboxError = class extends Error {
  status;
  operation;
  body;
  constructor(operation, status, message, body) {
    super(`${operation}: ${message}`);
    this.name = "SandboxError";
    this.status = status;
    this.operation = operation;
    this.body = body;
  }
};
async function toError(res, operation) {
  const text = await res.text();
  let body;
  try {
    body = ApiError.parse(JSON.parse(text));
  } catch {
    body = void 0;
  }
  const message = body?.error ?? text ?? res.statusText;
  return new SandboxError(operation, res.status, message, body);
}

// src/controller.ts
var DEFAULT_CONTROLLER_URL = "http://localhost:9000";
var SANDBOX_ID_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;
async function getOrCreateSandbox(id, config, opts = {}) {
  if (!SANDBOX_ID_PATTERN.test(id)) {
    throw new Error(
      `getOrCreateSandbox: id ${JSON.stringify(id)} must match ${SANDBOX_ID_PATTERN}`
    );
  }
  const validated = SandboxConfig.parse(config);
  const base = (opts.controllerUrl ?? DEFAULT_CONTROLLER_URL).replace(/\/+$/, "");
  const fetchImpl = opts.fetch ?? fetch;
  let res;
  try {
    res = await fetchImpl(`${base}/v1/sandboxes/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(validated)
    });
  } catch (err) {
    if (isConnectionRefused(err)) {
      throw new SandboxError(
        "getOrCreateSandbox",
        0,
        `controller is not reachable at ${base} (connection refused). Is it running?`
      );
    }
    throw err;
  }
  if (res.status !== 200 && res.status !== 201) {
    const text = await res.text();
    let body;
    try {
      body = ApiError.parse(JSON.parse(text));
    } catch {
      body = void 0;
    }
    throw new SandboxError(
      "getOrCreateSandbox",
      res.status,
      body?.error ?? text ?? res.statusText,
      body
    );
  }
  const ref = SandboxRef.parse(await res.json());
  return new Sandbox(ref, { controllerUrl: base, fetch: fetchImpl });
}
async function shutdown(sandbox) {
  const url = `${sandbox.controllerUrl}/v1/shutdown/${encodeURIComponent(sandbox.id)}`;
  let res;
  try {
    res = await sandbox.fetchImpl(url, { method: "POST" });
  } catch (err) {
    if (isConnectionRefused(err)) {
      throw new SandboxError(
        "shutdown",
        0,
        `controller is not reachable at ${sandbox.controllerUrl} (connection refused). Is it running?`
      );
    }
    throw err;
  }
  if (res.status === 204) return;
  throw await toError(res, "shutdown");
}
function isConnectionRefused(err) {
  if (!(err instanceof Error)) return false;
  const cause = err.cause;
  if (hasCode(cause, "ECONNREFUSED")) return true;
  if (cause instanceof AggregateError) {
    return cause.errors.some((e) => hasCode(e, "ECONNREFUSED"));
  }
  return false;
}
function hasCode(e, code) {
  return typeof e === "object" && e !== null && e.code === code;
}
export {
  ACLRule,
  ApiError,
  ApplyResult,
  Backend,
  Changes,
  ConfigApplyEvent,
  DEFAULT_CONTROLLER_URL,
  Egress,
  EgressRequestEvent,
  EgressResponseEvent,
  EgressRule,
  FSRequestEvent,
  FSResponseEvent,
  FileSystem,
  GDriveFileSystem,
  HttpMethod,
  LocalFileSystem,
  Sandbox,
  SandboxConfig,
  SandboxError,
  SandboxEvent,
  SandboxRef,
  StdioEvent,
  getOrCreateSandbox,
  shutdown
};
//# sourceMappingURL=index.js.map