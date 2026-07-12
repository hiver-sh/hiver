import {
  existsSync,
  lstatSync,
  mkdirSync,
  readlinkSync,
  rmSync,
  symlinkSync,
  unlinkSync,
} from "node:fs";
import { homedir } from "node:os";
import { delimiter, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { brand, bright, dim, red, white } from "../theme.js";

/**
 * Shared logic for wiring the Hiver Agent Skill (bundled at dist/skills/hiver)
 * into installed coding agents by symlinking it into each agent's skills dir.
 * Used by the `install-skill` command and offered during the first-run intro.
 */

export interface Agent {
  /** Display name. */
  name: string;
  /** Token used to filter on the command line (`hiver install-skill codex`). */
  key: string;
  /** Config dir name, under $HOME (global) or the project root (`--project`). */
  dir: string;
  /** Executable name, used to detect the agent when its config dir is absent. */
  bin: string;
}

// Skill-directory conventions taken from the agent images in docker/* — each
// agent scans `<config-dir>/skills/<name>` on startup.
export const AGENTS: Agent[] = [
  { name: "Claude Code", key: "claude", dir: ".claude", bin: "claude" },
  { name: "Codex", key: "codex", dir: ".codex", bin: "codex" },
  { name: "Antigravity", key: "antigravity", dir: ".antigravity", bin: "agy" },
  { name: "GitHub Copilot CLI", key: "copilot", dir: ".copilot", bin: "copilot" },
];

// Locate the bundled skill (dist/skills/hiver), whether we're running compiled
// from dist/ or through tsx from src/ (DEV). Returns null if not built yet.
export function resolveSkillSrc(): string | null {
  const here = dirname(fileURLToPath(import.meta.url));
  const candidates = [
    resolve(here, "../skills/hiver"), // dist/install-skill -> dist/skills
    resolve(here, "../../dist/skills/hiver"), // src/install-skill  -> dist/skills
  ];
  return candidates.find((p) => existsSync(join(p, "SKILL.md"))) ?? null;
}

function onPath(bin: string): boolean {
  const dirs = (process.env.PATH ?? "").split(delimiter).filter(Boolean);
  const exts = process.platform === "win32" ? ["", ".exe", ".cmd", ".bat"] : [""];
  return dirs.some((d) => exts.some((e) => existsSync(join(d, bin + e))));
}

/**
 * The agents installed on this machine — detected by the presence of their
 * $HOME config dir or their binary on PATH. `only` optionally restricts to a
 * set of agent keys.
 */
export function detectAgents(only?: Set<string>): Agent[] {
  const home = homedir();
  return AGENTS.filter(
    (a) =>
      (!only || only.has(a.key)) &&
      (existsSync(join(home, a.dir)) || onPath(a.bin)),
  );
}

// lstat without throwing — distinguishes "nothing there" from a (possibly
// dangling) symlink, which existsSync can't (it follows links).
function lstatOrNull(p: string) {
  try {
    return lstatSync(p);
  } catch {
    return null;
  }
}

export type LinkResult = "created" | "updated" | "exists" | "blocked";

export function linkSkill(target: string, src: string, force: boolean): LinkResult {
  mkdirSync(dirname(target), { recursive: true });
  const st = lstatOrNull(target);

  if (st?.isSymbolicLink()) {
    let current: string;
    try {
      current = resolve(dirname(target), readlinkSync(target));
    } catch {
      current = "";
    }
    // A symlink is already in place: if it already points at our skill it's
    // done, and otherwise we still skip it rather than fail — a pre-existing
    // link is treated as "already installed" unless the caller passes --force.
    if (current === src || !force) return "exists";
    // Repoint (forced): remove just the link with unlinkSync — rmSync throws
    // EISDIR ("Path is a directory") on a symlink that targets a directory.
    unlinkSync(target);
    symlinkSync(src, target, "dir");
    return "updated";
  }

  if (st) {
    // A real file/dir sits where the link should go — never clobber unasked.
    if (!force) return "blocked";
    rmSync(target, { recursive: true, force: true });
  }

  symlinkSync(src, target, "dir");
  return "created";
}

/**
 * Symlink the skill into each agent's skills dir, printing one status line per
 * agent. Writes under $HOME by default, or the current directory with
 * `project: true`. Returns the number of agents that could not be linked.
 */
export function installForAgents(
  src: string,
  agents: Agent[],
  opts: { project?: boolean; force?: boolean } = {},
): number {
  const home = homedir();
  const base = opts.project ? process.cwd() : home;
  let blocked = 0;

  for (const agent of agents) {
    const target = join(base, agent.dir, "skills", "hiver");
    const shown = target.replace(home, "~");
    try {
      const result = linkSkill(target, src, Boolean(opts.force));
      const note =
        result === "created"
          ? brand("linked")
          : result === "updated"
            ? brand("relinked")
            : result === "exists"
              ? dim("already linked")
              : red("blocked");
      const mark = result === "blocked" ? red("✖") : brand("✔");
      console.log(`  ${mark} ${white(agent.name.padEnd(20))} ${dim(shown)}  ${note}`);
      if (result === "blocked") {
        blocked++;
        console.log(
          `      ${dim(`a file or directory already exists there — rerun with ${white("--force")} to replace it`)}`,
        );
      }
    } catch (err) {
      blocked++;
      console.log(
        `  ${red("✖")} ${white(agent.name.padEnd(20))} ${dim(shown)}  ${red(String(err))}`,
      );
    }
  }

  if (blocked < agents.length) {
    console.log();
    console.log(
      `  ${bright("Skill installed!")} Ask your coding agent ${brand("`build an agent using agents sdk with hiver`")}`,
    );
  }

  return blocked;
}
