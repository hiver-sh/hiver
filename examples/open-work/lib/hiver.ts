import "server-only";

import {
  allowSandbox,
  getOrCreateSandbox,
  listSandboxes,
  Sandbox,
  type EgressRule,
  type SandboxConfig,
} from "@hiver.sh/client";

import { ORCHESTRATIONS, type Orchestration } from "./orchestration";
import type {
  ConversationMessage,
  ManifestEntry,
  OutputFile,
  UploadedFile,
} from "./types";

export const SANDBOX_IMAGE = "claude";

// The Hiver gateway the SDK talks to. Settable from the client's settings.
export const DEFAULT_GATEWAY_URL = "http://localhost:10000";
let gatewayUrl = DEFAULT_GATEWAY_URL;

export function setGatewayUrl(url?: string): void {
  if (url) gatewayUrl = url;
}
export function currentGatewayUrl(): string {
  return gatewayUrl;
}

// Each task gets its own sandbox (keyed by the task's 4-char id). Inside that
// dedicated sandbox the agent always runs under one fixed session id (Claude's),
// so the transcript path and resume are stable regardless of the outer key.
// Re-exported from the Claude provider, which owns it, for existing importers.
export { AGENT_SESSION_ID } from "./providers/claude";

// Both engines' key env vars are placeholders — the real key is injected on
// egress, never in the agent's env. Set at creation for whichever engine runs.
// BROWSER_SANDBOX_KEY tells the browser skill to use this task's own browser VM.
const envFor = (key: string): Record<string, string> => ({
  ANTHROPIC_API_KEY: "placeholder",
  OPENAI_API_KEY: "placeholder",
  BROWSER_SANDBOX_KEY: browserKeyFor(key),
});

// The gateway sandbox key is the task id suffixed, e.g. `a1b2-work`.
export const sandboxKeyFor = (taskId: string) => `${taskId}-work`;

// Task ids are short, unique, and pattern-safe.
const issuedIds = new Set<string>();

/** Generate a unique 4-char task id (also used as the sandbox key). */
export function newTaskId(): string {
  const abc = "abcdefghijklmnopqrstuvwxyz0123456789";
  let id: string;
  do {
    id = Array.from({ length: 4 }, () => abc[Math.floor(Math.random() * abc.length)]).join("");
  } while (issuedIds.has(id) || sandboxes.has(id));
  issuedIds.add(id);
  return id;
}

/** Egress rules that inject the engine's key into its provider host. */
export type Provisioning = { egress: EgressRule[] };

// The browser skill (in the `claude` image) drives Chrome in a nested sandbox.
// Give each task its own live instance, keyed by the task id, so concurrent
// tasks don't drive the same running browser. `allowSandbox` grants the
// gateway routes to create/proxy exactly that key, pinned to the `browser`
// image; the skill reads BROWSER_SANDBOX_KEY.
export const browserKeyFor = (taskKey: string) => `${taskKey}-browser`;

// The VM-state snapshot restore/save key, in contrast, is the SAME across every
// task: cookies, logins, and open tabs carry over from whichever task last used
// the browser, rather than each task cold-booting a blank one. This is
// independent of the per-task routing key above (isolation while running vs.
// shared state across runs).
const BROWSER_SNAPSHOT_KEY = "browser";

const browserConfig = (): SandboxConfig => ({
  image: "browser",
  snapshot: { vm: { key: BROWSER_SNAPSHOT_KEY } },
});

/**
 * The real key never enters the sandbox env — an egress override injects it
 * into the provider's API host. The task's per-task browser nested sandbox is
 * allowed via `allowSandbox`, and everything else is denied.
 */
export function provisioningFor(
  orchestration: Orchestration,
  apiKey: string,
  taskKey: string,
): Provisioning {
  const e = ORCHESTRATIONS[orchestration];
  // Extra provider hosts allowed without an override (e.g. codex's ChatGPT-login
  // hosts, which carry the token codex sets from its own stored OAuth session).
  const extra: EgressRule[] = (e.extraHosts ?? []).map((host) => ({
    access: "allow",
    host,
  }));
  return {
    egress: [
      {
        access: "allow",
        host: e.host,
        override: { headers: { [e.authHeader]: `${e.authPrefix}${apiKey}` } },
      },
      ...extra,
      // Also open the browser VM's /workspace file API so the skill's
      // write-file.js can stage local files for Chrome to read.
      ...allowSandbox(browserKeyFor(taskKey), browserConfig(), ["workspace"]),
      { access: "deny", host: "*" },
    ],
  };
}

// One sandbox per task, keyed by the task id. Only an image change rebuilds a
// sandbox; a key/egress change is applied in place via `applyConfig` so the
// filesystem — and the agent's persisted session transcript — survive.
/** Default sandbox TTL in seconds (30 min). */
export const DEFAULT_TTL_SECONDS = 1800;

type SandboxEntry = {
  image: string;
  baseEgress: EgressRule[];
  egressKey: string;
  ttl: number;
  sandbox: Sandbox;
};
const sandboxes = new Map<string, SandboxEntry>();

// Hosts the user granted egress to per task (added on top of the base rules).
const grantedHosts = new Map<string, Set<string>>();

// Nested sandboxes a task spawned (e.g. the browser VM), keyed taskKey → (id → key).
// Discovered from egress.response x-hiver-sandbox-id/key headers; torn down with
// the task so dependencies don't outlive it.
const dependencies = new Map<string, Map<string, string>>();

/** Track a nested sandbox spawned by a task (from its egress response headers). */
export function addSandboxDependency(taskKey: string, id: string, key: string): void {
  let deps = dependencies.get(taskKey);
  if (!deps) {
    deps = new Map();
    dependencies.set(taskKey, deps);
  }
  deps.set(id, key);
}

/** Insert `allow` rules for granted hosts before the trailing deny-all. */
function injectAllows(base: EgressRule[], hosts?: Set<string>): EgressRule[] {
  if (!hosts || hosts.size === 0) return base;
  const rules = [...base];
  const denyIdx = rules.findIndex((r) => r.access === "deny" && r.host === "*");
  const inserts: EgressRule[] = [...hosts].map((host) => ({ access: "allow", host }));
  if (denyIdx >= 0) rules.splice(denyIdx, 0, ...inserts);
  else rules.push(...inserts);
  return rules;
}

/**
 * Get (or create) the task's sandbox (keyed by `key`). Every call goes through
 * `getOrCreateSandbox`, so a sandbox that was shut down (ttl expiry) or never
 * existed is recreated from the desired config. An image change rebuilds it;
 * egress (the API key) and ttl are applied in place via `applyConfig` on an
 * already-running sandbox — together, so changing one preserves the other.
 * Callers that omit a field reuse the last-known value.
 */
export async function getSandbox(
  key: string,
  image?: string,
  provisioning?: Provisioning,
  ttlSeconds?: number,
): Promise<Sandbox> {
  const entry = sandboxes.get(key);
  const target = image ?? entry?.image ?? SANDBOX_IMAGE;
  const baseEgress = provisioning?.egress ?? entry?.baseEgress ?? [];
  const egress = injectAllows(baseEgress, grantedHosts.get(key));
  const egressKey = JSON.stringify(egress);
  const desiredTtl = ttlSeconds ?? entry?.ttl ?? DEFAULT_TTL_SECONDS;

  // Image change → the engine's whole runtime differs, so tear down first.
  if (entry && entry.image !== target) {
    try {
      await entry.sandbox.shutdown();
    } catch {
      /* already gone */
    }
    sandboxes.delete(key);
  }

  const prevId = sandboxes.get(key)?.sandbox.id;

  // The full desired config. `applyConfig` reconciles the WHOLE config — it
  // drops any field not present — so we always send this complete object, never
  // a partial patch (which would strip fs/snapshot). A files snapshot (keyed by
  // the sandbox key, captured on shutdown) persists /workspace and the agent's
  // transcript across a ttl shutdown → restore.
  const config: SandboxConfig = {
    image: target,
    env: envFor(key),
    egress,
    ttl: desiredTtl,
    fs: [{ backend: "local", mount: "/workspace" }],
    snapshot: { files: { key: sandboxKeyFor(key), write_on_shutdown: true } },
  };

  // Recreates the sandbox (with this config) if it was shut down or is missing.
  const sandbox = await getOrCreateSandbox(sandboxKeyFor(key), config, {
    // Provisioning a fresh image can outlast the default 60s.
    timeoutMs: 0,
    gatewayUrl,
  });

  // If the sandbox already existed (same id), apply egress/ttl changes in place.
  // A freshly created one already has the desired config.
  if (prevId && prevId === sandbox.id) {
    const prev = sandboxes.get(key)!;
    if (prev.egressKey !== egressKey || prev.ttl !== desiredTtl) {
      await sandbox.applyConfig(config); // FULL config, not a partial patch
    }
  }

  sandboxes.set(key, { image: target, baseEgress, egressKey, ttl: desiredTtl, sandbox });
  return sandbox;
}

/**
 * Grant egress to a host for a task and propagate it to the whole tree: the
 * parent (main) sandbox and every dependent (nested) sandbox it spawned.
 */
export async function allowEgress(key: string, host: string): Promise<void> {
  let set = grantedHosts.get(key);
  if (!set) {
    set = new Set();
    grantedHosts.set(key, set);
  }
  set.add(host);
  await getSandbox(key); // apply to the parent sandbox in place

  // Propagate to nested sandboxes (e.g. the browser VM) — the denial often
  // originates there, so they need the grant too.
  const deps = dependencies.get(key);
  if (deps) {
    await Promise.all(
      [...deps].map(async ([id, depKey]) => {
        try {
          const nested = new Sandbox({ id, key: depKey }, { gatewayUrl });
          const cfg = await nested.getConfig();
          if (cfg.egress?.some((r) => r.access === "allow" && r.host === host)) {
            return; // already granted
          }
          const egress = injectAllows(cfg.egress ?? [], new Set([host]));
          await nested.applyConfig({ ...cfg, egress });
        } catch {
          /* nested gone */
        }
      }),
    );
  }
}

/** The image a task's sandbox was provisioned from. */
export function currentSandboxImage(key: string): string {
  return sandboxes.get(key)?.image ?? SANDBOX_IMAGE;
}

/** CDP WebSocket URL for a browser sandbox handle (stable `/cdp` alias on 9223). */
const cdpUrlFor = (sandbox: Sandbox): string =>
  sandbox.proxyUrl(9223).replace(/^http/, "ws") + "cdp";

/**
 * Resolve the CDP WebSocket URL for a task's browser VM from our local
 * dependency map. Fast, but the map is dropped on server restart / HMR — use
 * {@link resolveBrowserCdpUrl} to reconcile against Hiver when it comes up empty.
 */
export function browserCdpUrl(taskKey: string): string | null {
  const deps = dependencies.get(taskKey);
  if (!deps) return null;
  const wantKey = browserKeyFor(taskKey);
  for (const [id, depKey] of deps) {
    if (depKey !== wantKey) continue;
    return cdpUrlFor(new Sandbox({ id, key: depKey }, { gatewayUrl }));
  }
  return null;
}

/**
 * Resolve the task's browser VM CDP URL, treating Hiver as the source of truth:
 * query the live sandbox list, reconcile the match into our local dependency map
 * (or drop a stale one Hiver no longer reports), and return its CDP URL — or null
 * when Hiver has no such sandbox. Falls back to the local map if the query fails.
 */
export async function resolveBrowserCdpUrl(taskKey: string): Promise<string | null> {
  const wantKey = browserKeyFor(taskKey);
  let all: Sandbox[];
  try {
    all = await listSandboxes({ gatewayUrl });
  } catch {
    return browserCdpUrl(taskKey);
  }

  const browser = all.find((s) => s.key === wantKey);
  const deps = dependencies.get(taskKey);
  if (!browser) {
    // Hiver no longer has it → forget any stale local entry.
    if (deps) for (const [id, k] of deps) if (k === wantKey) deps.delete(id);
    return null;
  }
  addSandboxDependency(taskKey, browser.id, browser.key); // reconcile in
  return cdpUrlFor(browser);
}

/**
 * Shut down the task's browser VM (keyed `${taskKey}-browser`), resolving it
 * from Hiver so it works even when the local dependency map is empty. Keeps a
 * task's browser from outliving it and being re-matched by a later task.
 */
async function shutdownBrowser(taskKey: string): Promise<void> {
  const wantKey = browserKeyFor(taskKey);
  try {
    const all = await listSandboxes({ gatewayUrl });
    const browser = all.find((s) => s.key === wantKey);
    if (browser) await browser.shutdown();
  } catch {
    /* already gone or unreachable */
  }
}

/** Shut down and forget a task's sandbox + its nested dependencies. */
export async function shutdownSandbox(key: string): Promise<void> {
  // Tear down nested sandboxes the task spawned (e.g. the browser VM).
  const deps = dependencies.get(key);
  dependencies.delete(key);
  if (deps) {
    await Promise.all(
      [...deps].map(async ([id, depKey]) => {
        try {
          await new Sandbox({ id, key: depKey }, { gatewayUrl }).shutdown();
        } catch {
          /* already gone */
        }
      }),
    );
  }
  // Belt-and-suspenders: also tear down the browser VM by its guid key in case
  // it wasn't in the dependency map (e.g. after a server restart).
  await shutdownBrowser(key);

  let sandbox = sandboxes.get(key)?.sandbox;
  sandboxes.delete(key);
  issuedIds.delete(key);
  try {
    if (!sandbox) sandbox = await getSandbox(key);
  } catch {
    return; // unreachable — nothing to tear down
  }
  sandboxes.delete(key);

  // Drop the files snapshot so `write_on_shutdown` doesn't persist a deleted
  // task's workspace on the way out. applyConfig reconciles the WHOLE config, so
  // send the current config with only the snapshot cleared.
  try {
    const current = await sandbox.getConfig();
    await sandbox.applyConfig({ ...current, snapshot: {} });
  } catch {
    /* ignore */
  }
  try {
    await sandbox.shutdown();
  } catch {
    /* already gone */
  }
}

/**
 * Rebuild the task's conversation from whichever engine's transcript the sandbox
 * has. Each provider knows its own format; the first one that reports a
 * transcript wins. Engine-agnostic so `/api/conversation` needn't know which
 * engine ran (the in-memory registry that would say is gone after a refresh).
 */
export async function readConversation(
  key: string,
): Promise<ConversationMessage[]> {
  const sandbox = await getSandbox(key);
  for (const provider of Object.values(ORCHESTRATIONS)) {
    const { resume } = await provider.resumeInfo(sandbox, key);
    if (resume) return provider.readConversation(sandbox, key);
  }
  return [];
}

/**
 * Push each uploaded file into `/workspace` so the task's `@references`
 * resolve to real files. Returns the files that landed.
 */
export async function uploadTaskFiles(
  key: string,
  files: File[],
  manifest: ManifestEntry[],
  image?: string,
  provisioning?: Provisioning,
): Promise<UploadedFile[]> {
  const sandbox = await getSandbox(key, image, provisioning);
  await ensureWorkspaceDirs(sandbox);
  const uploaded: UploadedFile[] = [];

  for (let i = 0; i < files.length; i++) {
    const entry = manifest[i];
    if (!entry) continue;
    // Input files live under /workspace/input (see SYSTEM_PROMPT).
    const path = `/workspace/input/${entry.name}`;
    const bytes = new Uint8Array(await files[i].arrayBuffer());
    await sandbox.writeFile(path, bytes);
    uploaded.push({ ...entry, path });
  }

  return uploaded;
}

/** Ignore agent temp files, e.g. `poem.md.tmp.20.216513c7c3d5`. */
export function isTempFile(name: string): boolean {
  return /\.tmp(\.|$)/.test(name);
}

// Only surface files the viewer can render: markdown, text, and images.
const VIEWABLE_EXT = new Set([
  "md", "markdown", "txt", "png", "jpg", "jpeg", "gif", "webp", "svg", "avif", "bmp",
]);

/** Whether a file should be surfaced to the client (viewable + not a temp file). */
export function isViewableFile(name: string): boolean {
  if (isTempFile(name)) return false;
  const ext = name.split(".").pop()?.toLowerCase() ?? "";
  return VIEWABLE_EXT.has(ext);
}

// Telemetry / noise hosts to silently deny — never prompt the user to allow them.
// Matched by exact host or as a suffix (so all subdomains are covered).
const SKIP_PERMISSION_HOSTS = [
  "datadoghq.com", // e.g. http-intake.logs.us5.datadoghq.com
  "sentry.io",
  "statsig.com",
  "statsig.anthropic.com",
];

/** Whether an egress denial to this host should be suppressed (no elicitation). */
export function skipEgressPermission(host: string): boolean {
  return SKIP_PERMISSION_HOSTS.some(
    (h) => host === h || host.endsWith(`.${h}`),
  );
}

/** Create the input/output convention dirs (idempotent). */
export async function ensureWorkspaceDirs(sandbox: Sandbox): Promise<void> {
  await sandbox.exec(["mkdir", "-p", "/workspace/input", "/workspace/output"], {
    cwd: "/workspace",
  });
}

/**
 * List the task's current input/output files via the sandbox directory API.
 * Used on refresh, when the live fs events are gone.
 */
export async function listWorkspaceFiles(
  key: string,
): Promise<{ inputs: OutputFile[]; outputs: OutputFile[] }> {
  const sandbox = await getSandbox(key);
  const list = async (dir: string): Promise<OutputFile[]> => {
    try {
      const entries = await sandbox.listDirectory(dir);
      return entries
        .filter((e) => !e.is_dir && isViewableFile(e.name))
        .map((e) => ({ name: e.name, path: e.path, size: e.size }));
    } catch {
      return [];
    }
  };
  const [inputs, outputs] = await Promise.all([
    list("/workspace/input"),
    list("/workspace/output"),
  ]);
  return { inputs, outputs };
}
