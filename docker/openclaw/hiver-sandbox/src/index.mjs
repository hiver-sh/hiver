// OpenClaw sandbox backend backed by a Hiver sandbox.
//
// Registering the "hiver" backend makes OpenClaw route every sandboxed tool —
// `exec`/`process` (via buildExecSpec) and the filesystem tools `read`/`write`/
// `edit`/`apply_patch` (via the fs bridge) — into a per-scope Hiver sandbox
// instead of a local Docker container. The Gateway itself keeps running on the
// host; only tool execution crosses into the sandbox.
//
// Written as plain ESM so it can be baked into the image and loaded with
// `openclaw plugins install` without a TypeScript build step.

import path from "node:path";
import { promises as fsp } from "node:fs";
import { fileURLToPath } from "node:url";

import { definePluginEntry } from "openclaw/plugin-sdk/plugin-entry";
import {
  registerSandboxBackend,
  sanitizeEnvVars,
} from "openclaw/plugin-sdk/sandbox";
import { getOrCreateSandbox, resolveGatewayUrl } from "@hiver.sh/client";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const EXEC_SHIM_PATH = path.join(HERE, "exec-shim.mjs");

// Agent-visible mount root inside a Hiver sandbox. `getOrCreateSandbox`'s
// default config mounts a rw `local` backend at `/workspace`, so that is the
// working directory tools resolve relative paths against.
const SANDBOX_WORKDIR = "/workspace";

// Logical Hiver image the nested sandbox boots. Overridable so an operator can
// point tool execution at a heavier image (browser, language toolchains, …).
const SANDBOX_IMAGE = process.env.HIVER_SANDBOX_IMAGE || "node";

/**
 * Collapse an OpenClaw scope key (which can contain `/`, `:`, spaces, …) into a
 * Hiver sandbox key. Hiver keys must match `^[A-Za-z0-9_-]{1,64}$`, and the same
 * scope must always map to the same key so exec, fs, and probes share one
 * sandbox. A short hash suffix keeps distinct scopes from colliding after the
 * lossy character replacement.
 */
function deriveHiverKey(scopeKey) {
  const trimmed = (scopeKey || "").trim() || "session";
  let hash = 5381;
  for (const ch of trimmed) hash = ((hash * 33) ^ ch.charCodeAt(0)) >>> 0;
  const safe = trimmed
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 40);
  return `oc-${safe || "session"}-${hash.toString(16).padStart(8, "0")}`;
}

// Plugin-scoped logger, set from register(api) so factory/exec/fs paths can log
// to the gateway log. Falls back to console so the exec shim (a separate
// process, where api is unavailable) still emits something.
let log = {
  info: (m) => console.error(`[hiver-sandbox] ${m}`),
  warn: (m) => console.error(`[hiver-sandbox] ${m}`),
  error: (m) => console.error(`[hiver-sandbox] ${m}`),
};

// One live Hiver handle per derived key, shared across every backend handle
// created for that scope so we do not re-provision on each tool call.
const sandboxCache = new Map();

// Scopes whose host workspace we have already copied into the sandbox this
// process, so the (one-time) seed does not re-run every tool call.
const seededKeys = new Set();

// Directories never worth copying into the sandbox workspace.
const SEED_SKIP_DIRS = new Set([".git", "node_modules"]);
// Skip individual files larger than this (skills are text; guards against a
// stray large asset ballooning the per-file upload).
const SEED_MAX_FILE_BYTES = 5 * 1024 * 1024;

// Recursively list files under `dir` (absolute paths), skipping junk dirs.
async function collectWorkspaceFiles(dir) {
  const out = [];
  const walk = async (current) => {
    let entries;
    try {
      entries = await fsp.readdir(current, { withFileTypes: true });
    } catch {
      return;
    }
    for (const entry of entries) {
      const abs = path.join(current, entry.name);
      if (entry.isDirectory()) {
        if (SEED_SKIP_DIRS.has(entry.name)) continue;
        await walk(abs);
      } else if (entry.isFile()) {
        out.push(abs);
      }
    }
  };
  await walk(dir);
  return out;
}

/**
 * Copy OpenClaw's host-seeded workspace (bootstrap files + materialized
 * `skills/`) into the sandbox's `/workspace` once, so files OpenClaw advertises
 * at `/workspace/...` (e.g. `/workspace/skills/<name>/SKILL.md`) actually exist
 * on disk for the agent's read/exec. OpenClaw seeds these on the host assuming a
 * bind-mount; the Hiver backend has no bind-mount, so we bridge it here.
 *
 * Idempotent + non-clobbering via a marker file: once written, later runs (incl.
 * after a gateway restart that reuses the same sandbox) skip the copy so the
 * agent's own edits are preserved.
 */
async function seedWorkspaceIfNeeded(sandbox, hostDir, containerRoot) {
  const marker = path.posix.join(containerRoot, ".hiver-seeded");
  try {
    await sandbox.readFile(marker);
    return "already-seeded";
  } catch {
    /* not seeded yet */
  }
  const files = await collectWorkspaceFiles(hostDir);
  let copied = 0;
  for (const abs of files) {
    let content;
    try {
      const stat = await fsp.stat(abs);
      if (stat.size > SEED_MAX_FILE_BYTES) continue;
      content = await fsp.readFile(abs);
    } catch {
      continue;
    }
    const rel = path.relative(hostDir, abs).split(path.sep).join("/");
    await sandbox.writeFile(path.posix.join(containerRoot, rel), content);
    copied += 1;
  }
  await sandbox.writeFile(
    marker,
    `seeded ${new Date().toISOString()} (${copied} files from host workspace)\n`,
  );
  return `seeded ${copied} files`;
}

function ensureSandbox(hiverKey, gatewayUrl) {
  let pending = sandboxCache.get(hiverKey);
  if (!pending) {
    const config = {
      ...(SANDBOX_IMAGE ? { image: SANDBOX_IMAGE } : {}),
      // Persist the sandbox filesystem across recreations, keyed by the same id
      // OpenClaw uses for this scope (our runtimeId/containerName == hiverKey).
      // So when OpenClaw recreates the sandbox for a scope, /workspace is
      // restored from the last shutdown's snapshot.
      //   - key: restore key == hiverKey (the OpenClaw-facing sandbox id).
      //   - write_on_shutdown: capture on shutdown/termination (without this the
      //     snapshot is never written and the restore key is inert).
      //   - include: /workspace is an sbxfuse `local` mount, which is NOT
      //     captured by an empty include (that only grabs the overlay rootfs) —
      //     it must be listed explicitly.
      snapshot: {
        files: {
          key: hiverKey,
          write_on_shutdown: true,
          include: ["/workspace"],
        },
      },
    };
    pending = getOrCreateSandbox(hiverKey, config, { gatewayUrl }).catch(
      (err) => {
        // Do not cache a failed provision — let the next call retry.
        sandboxCache.delete(hiverKey);
        throw err;
      },
    );
    sandboxCache.set(hiverKey, pending);
  }
  return pending;
}

// Build a mapper from OpenClaw's host-side paths to paths inside the Hiver
// sandbox. This is the crux of keeping the file tools and exec consistent:
// OpenClaw resolves read/write/edit paths against the HOST workspace dir
// (`sandbox.workspaceDir`, e.g. ~/.openclaw/sandboxes/<scope>) and hands the
// bridge absolute host paths, but exec runs in the sandbox's container workdir
// (`/workspace`). Without translating `<hostWorkspaceRoot>/x` → `/workspace/x`,
// a file written by `write` lands somewhere exec never looks. Mirrors what the
// built-in Docker/SSH bridges do via bind-mount / remote workspace roots.
function makeContainerPathMapper(ctx) {
  const containerRoot = (ctx && ctx.containerWorkdir) || SANDBOX_WORKDIR;
  // Host roots OpenClaw resolves file-tool paths against, longest first so a
  // nested root (agent workspace under the sandbox workspace) wins.
  const hostRoots = [ctx?.workspaceDir, ctx?.agentWorkspaceDir]
    .filter((p) => typeof p === "string" && p.length > 0)
    .map((p) => path.resolve(p))
    .sort((a, b) => b.length - a.length);

  const map = (filePath, cwd) => {
    const base =
      cwd && cwd.length > 0 ? cwd : hostRoots[0] || containerRoot;
    const abs = path.isAbsolute(filePath)
      ? filePath
      : path.resolve(base, filePath);
    for (const root of hostRoots) {
      const rel = path.relative(root, abs);
      if (rel === "") return containerRoot;
      if (rel && !rel.startsWith("..") && !path.isAbsolute(rel)) {
        return path.posix.join(containerRoot, rel.split(path.sep).join("/"));
      }
    }
    // Already a container-absolute path (e.g. /workspace/x, /tmp/x): normalize.
    return path.posix.normalize(abs.split(path.sep).join("/"));
  };
  map.containerRoot = containerRoot;
  return map;
}

// Run a shell script in the sandbox with positional args (referenced as $1, $2…
// in the script so no manual escaping is needed). Returns buffered output.
async function runScript(sandbox, script, args = [], opts = {}) {
  const res = await sandbox.exec(
    ["/bin/sh", "-c", script, "openclaw-hiver-sandbox", ...args],
    { signal: opts.signal },
  );
  return {
    stdout: Buffer.from(res.stdout ?? "", "utf8"),
    stderr: Buffer.from(res.stderr ?? "", "utf8"),
    code: res.exit_code,
  };
}

/**
 * Filesystem bridge that fulfils the OpenClaw `SandboxFsBridge` contract using
 * Hiver's native file API for binary-safe reads/writes and shell commands for
 * directory/stat/rename operations. Every path is an absolute sandbox path, so
 * files created here land in the Hiver sandbox, not on the Gateway host.
 */
function createHiverFsBridge(sandboxPromise, ctx) {
  const withSandbox = async (fn) => fn(await sandboxPromise);
  const toContainerPath = makeContainerPathMapper(ctx);
  const containerRoot = toContainerPath.containerRoot;

  return {
    resolvePath({ filePath, cwd }) {
      const containerPath = toContainerPath(filePath, cwd);
      const relativePath = path.posix.relative(containerRoot, containerPath);
      return { relativePath, containerPath };
    },

    async readFile({ filePath, cwd }) {
      const containerPath = toContainerPath(filePath, cwd);
      return withSandbox(async (sandbox) => {
        const bytes = await sandbox.readFile(containerPath);
        return Buffer.from(bytes);
      });
    },

    async writeFile({ filePath, cwd, data, encoding }) {
      const containerPath = toContainerPath(filePath, cwd);
      const buffer = Buffer.isBuffer(data)
        ? data
        : Buffer.from(data, encoding ?? "utf8");
      // Hiver's file store does MkdirAll(dirname) before writing, so parents are
      // created automatically — no separate `mkdir -p` round-trip needed. That
      // keeps the write to a single request (matching bash exec's latency) so a
      // write turn doesn't lag enough to trip the client's resend/session race.
      return withSandbox((sandbox) => sandbox.writeFile(containerPath, buffer));
    },

    async mkdirp({ filePath, cwd, signal }) {
      const containerPath = toContainerPath(filePath, cwd);
      return withSandbox(async (sandbox) => {
        const res = await runScript(sandbox, 'mkdir -p -- "$1"', [containerPath], {
          signal,
        });
        if (res.code !== 0) {
          throw new Error(
            `hiver sandbox mkdirp failed for ${containerPath}: ${res.stderr.toString("utf8").trim()}`,
          );
        }
      });
    },

    async remove({ filePath, cwd, recursive, force, signal }) {
      const containerPath = toContainerPath(filePath, cwd);
      const flags = `${recursive ? "r" : ""}${force ? "f" : ""}`;
      const script = flags ? `rm -${flags} -- "$1"` : 'rm -- "$1"';
      return withSandbox(async (sandbox) => {
        const res = await runScript(sandbox, script, [containerPath], { signal });
        if (res.code !== 0 && !force) {
          throw new Error(
            `hiver sandbox remove failed for ${containerPath}: ${res.stderr.toString("utf8").trim()}`,
          );
        }
      });
    },

    async rename({ from, to, cwd, signal }) {
      const fromPath = toContainerPath(from, cwd);
      const toPath = toContainerPath(to, cwd);
      return withSandbox(async (sandbox) => {
        const res = await runScript(
          sandbox,
          'mkdir -p -- "$(dirname -- "$2")" && mv -- "$1" "$2"',
          [fromPath, toPath],
          { signal },
        );
        if (res.code !== 0) {
          throw new Error(
            `hiver sandbox rename failed (${fromPath} -> ${toPath}): ${res.stderr.toString("utf8").trim()}`,
          );
        }
      });
    },

    async stat({ filePath, cwd, signal }) {
      const containerPath = toContainerPath(filePath, cwd);
      return withSandbox(async (sandbox) => {
        // %F = human type, %s = size in bytes, %Y = mtime (seconds).
        const res = await runScript(
          sandbox,
          'stat -c "%F|%s|%Y" -- "$1" 2>/dev/null',
          [containerPath],
          { signal },
        );
        if (res.code !== 0) return null;
        const [kind, size, mtimeSec] = res.stdout.toString("utf8").trim().split("|");
        const type =
          kind === "directory"
            ? "directory"
            : kind === "regular file" || kind === "regular empty file"
              ? "file"
              : "other";
        return {
          type,
          size: Number.parseInt(size, 10) || 0,
          mtimeMs: (Number.parseInt(mtimeSec, 10) || 0) * 1000,
        };
      });
    },
  };
}

/**
 * Sandbox backend factory. OpenClaw calls this once per sandbox scope; the
 * returned handle exposes exec (buildExecSpec/runShellCommand) and the fs bridge.
 */
async function createHiverSandboxBackend(params) {
  // `|| undefined` so an empty env value falls back to the client default
  // instead of resolving to "" (nullish coalescing wouldn't catch "").
  const gatewayUrl = resolveGatewayUrl(process.env.HIVER_GATEWAY_URL || undefined);
  const hiverKey = deriveHiverKey(params.scopeKey);
  // This line proves the sandbox path engaged for the turn. If you run an exec
  // and DON'T see it in the gateway log, the session was not sandboxed (check
  // agents.defaults.sandbox.mode/backend) — the command ran on the host.
  log.info(
    `provisioning sandbox: scope=${params.scopeKey} key=${hiverKey} gateway=${gatewayUrl}`,
  );
  const sandboxPromise = ensureSandbox(hiverKey, gatewayUrl);
  // Provision eagerly so the first exec/read does not pay the cold-start.
  try {
    await sandboxPromise;
    log.info(`sandbox ready: key=${hiverKey}`);
  } catch (err) {
    log.error(
      `sandbox provision FAILED for key=${hiverKey} at ${gatewayUrl}: ${String(err?.message || err)}. ` +
        `Set HIVER_GATEWAY_URL to a reachable Hiver control plane.`,
    );
    throw err;
  }

  // Copy OpenClaw's host-seeded workspace (bootstrap files + skills) into the
  // sandbox so `/workspace/skills/...`, SOUL.md, etc. actually exist for the
  // agent. Best-effort and one-time per scope; a failure must not break the turn.
  if (params.workspaceDir && !seededKeys.has(hiverKey)) {
    try {
      const result = await seedWorkspaceIfNeeded(
        await sandboxPromise,
        params.workspaceDir,
        SANDBOX_WORKDIR,
      );
      seededKeys.add(hiverKey);
      log.info(`workspace seed (${hiverKey}): ${result} from ${params.workspaceDir}`);
    } catch (err) {
      log.warn(
        `workspace seed failed for ${hiverKey}: ${String(err?.message || err)}`,
      );
    }
  }

  return {
    id: "hiver",
    runtimeId: hiverKey,
    runtimeLabel: hiverKey,
    workdir: SANDBOX_WORKDIR,
    configLabel: gatewayUrl,
    configLabelKind: "Gateway",

    // exec/process: OpenClaw spawns this argv locally as a child and pipes its
    // stdio. The shim reconnects to the sandbox and streams the command through.
    async buildExecSpec({ command, workdir, env, usePty }) {
      const shimEnv = {
        ...sanitizeEnvVars(process.env).allowed,
        HIVER_GATEWAY_URL: gatewayUrl,
        HIVER_SANDBOX_KEY: hiverKey,
        // Same image the factory created the key with, so the shim's
        // get-or-create routes to the same pod (the gateway hashes on image).
        HIVER_SANDBOX_IMAGE: SANDBOX_IMAGE ?? "",
        HIVER_EXEC_COMMAND: command,
        HIVER_EXEC_WORKDIR: workdir ?? SANDBOX_WORKDIR,
        HIVER_EXEC_ENV: JSON.stringify(env ?? {}),
        HIVER_EXEC_TTY: usePty ? "1" : "0",
      };
      return {
        argv: [process.execPath, EXEC_SHIM_PATH],
        env: shimEnv,
        stdinMode: "pipe-open",
      };
    },

    // Buffered shell command used for fs probes and OpenClaw internals.
    async runShellCommand({ script, args, signal }) {
      const sandbox = await sandboxPromise;
      return runScript(sandbox, script, args ?? [], { signal });
    },

    // `sandbox` here is OpenClaw's SandboxFsBridgeContext, carrying the host
    // workspaceDir/agentWorkspaceDir and containerWorkdir the mapper needs.
    createFsBridge: ({ sandbox }) => createHiverFsBridge(sandboxPromise, sandbox),
  };
}

export default definePluginEntry({
  id: "hiver-sandbox",
  name: "Hiver Sandbox",
  description:
    "Sandbox backend that runs OpenClaw exec and filesystem tools inside a Hiver sandbox.",
  register(api) {
    if (api.logger) log = api.logger;
    registerSandboxBackend("hiver", {
      factory: createHiverSandboxBackend,
      resolveWorkdir: () => SANDBOX_WORKDIR,
    });
    log.info?.("hiver-sandbox: registered 'hiver' sandbox backend");
  },
});
