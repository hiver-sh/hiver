import { homedir } from "node:os";
import { subcommand, run } from "../args.js";
import { bold, dim, red, white } from "../theme.js";
import { AGENTS, detectAgents, installForAgents, resolveSkillSrc } from "./install.js";

/**
 * `hiver install-skill` — wire the Hiver Agent Skill bundled with this CLI into
 * whatever coding agents are installed on this machine, by symlinking the
 * shipped `skills/hiver` directory into each agent's skills directory.
 */

const cmd = subcommand(
  "install-skill",
  "Symlink the bundled Hiver skill into installed coding agents (claude, codex, antigravity, copilot).",
)
  .argument("[agents...]", "limit to specific agents (default: all detected)")
  .option("--project", "install into ./<agent>/skills in the current directory instead of $HOME")
  .option("--force", "replace a non-symlink file/dir already at the skill path");
run(cmd);

const opts = cmd.opts<{ project?: boolean; force?: boolean }>();

const src = resolveSkillSrc();
if (!src) {
  console.error(
    `\n  ${red("✖")} bundled skill not found — run ${white("npm run build")} in the cli package first.\n`,
  );
  process.exit(1);
}

// Optional positional filter: `hiver install-skill codex copilot`.
const wanted = new Set(cmd.args.map((a) => a.toLowerCase()));
const unknown = [...wanted].filter((w) => !AGENTS.some((a) => a.key === w));
if (unknown.length > 0) {
  console.error(
    `\n  ${red("✖")} unknown agent(s): ${unknown.join(", ")} — choose from ${AGENTS.map((a) => a.key).join(", ")}\n`,
  );
  process.exit(1);
}

const detected = detectAgents(wanted.size > 0 ? wanted : undefined);

console.log();
console.log(`  ${bold(white("Install Hiver skill"))}  ${dim(src.replace(homedir(), "~"))}\n`);

if (detected.length === 0) {
  const scope =
    wanted.size > 0
      ? [...wanted].join(", ")
      : `any supported agent (${AGENTS.map((a) => a.key).join(", ")})`;
  console.log(`  ${dim(`No ${scope} detected — nothing to do.`)}\n`);
  process.exit(0);
}

const blocked = installForAgents(src, detected, opts);
console.log();
process.exit(blocked > 0 ? 1 : 0);
