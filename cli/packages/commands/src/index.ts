import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { COMMANDS } from "./commands.js";
import { HIVER_DIR } from "./config.js";
import { bold, dim, red, white } from "./theme.js";
import { playIntro, staticLogo, type HiveLogo } from "./hive.js";
import { confirm } from "./prompt.js";
import { detectAgents, installForAgents, resolveSkillSrc } from "./install-skill/install.js";

// The `hiver` entry, for spawning subcommands (e.g. `start`). One level up from
// both src/ and dist/.
const BIN = resolve(dirname(fileURLToPath(import.meta.url)), "../bin.js");

// Start an example agent (the CLI's own `start`) under a known key, inheriting
// stdio so its output shows. Used on the intro/first-run path so the inspector
// can open straight onto this running sandbox. Best-effort: failure shouldn't
// block the intro.
function runStart(key: string): Promise<boolean> {
  return new Promise((res) => {
    const child = spawn(process.execPath, [BIN, "start", key], {
      stdio: "inherit",
    });
    child.on("error", () => res(false));
    child.on("exit", (code) => res(code === 0));
  });
}

// First-run offer: if any coding agents are installed, ask whether to symlink
// the bundled Hiver skill into them. Best-effort — declining (or no agents /
// no bundled skill) just skips it.
async function offerSkillInstall(): Promise<void> {
  const src = resolveSkillSrc();
  if (!src) return;
  const agents = detectAgents();
  if (agents.length === 0) return;

  console.log();
  const yes = await confirm(
    `  ${white("Install the Hiver skill")} into ${bold(agents.map((a) => a.name).join(", "))}?`,
  );
  if (!yes) return;

  console.log();
  installForAgents(src, agents, {});
}

/**
 * Default entry point: animate the `hiver` logo, then print the available
 * subcommands. The logo animation lives in ./hive.js (see `playIntro`).
 */

// Branding for the logo, with the version read from the published package
// (cli/package.json, three levels up from both src/ and dist/).
function logoMeta(): HiveLogo {
  const __dirname = dirname(fileURLToPath(import.meta.url));
  const { version } = JSON.parse(
    readFileSync(resolve(__dirname, "../../../package.json"), "utf8"),
  ) as { version: string };
  return { name: "Hiver", tagline: "Agent Runtime", version };
}

function printCommands(unknown: string | undefined) {
  console.log();

  if (unknown && !unknown.startsWith("-")) {
    console.log(`  ${red("✖")} unknown command: ${bold(unknown)}\n`);
  }

  console.log(`  ${dim("Usage:")} ${white("hiver")} ${dim("<command> [options]")}\n`);
  console.log(`  ${bold(white("Commands"))}`);

  const pad = Math.max(...COMMANDS.map((c) => c.name.length));
  for (const cmd of COMMANDS) {
    console.log(`    ${white(cmd.name.padEnd(pad))}  ${dim(cmd.summary)}`);
  }

  console.log();
  console.log(
    `  ${dim("Run")} hiver ${dim("<command> --help for command details.")}`,
  );
  console.log();
}

const unknown = process.argv[2];
// `--intro`, or a first run (no ~/.hiver yet), plays the intro and launches the
// inspector.
const introFlag = process.argv.slice(2).includes("--intro");
const firstRun = !existsSync(HIVER_DIR);
const interactive =
  Boolean(process.stdout.isTTY) && !process.env.CI && !process.env.NO_COLOR;
// Intro only makes sense in an interactive terminal — skip on CI or pipes.
const intro = (introFlag || firstRun) && interactive;

console.log();
if (intro) {
  // Restore the cursor if interrupted mid-animation.
  const restore = () => {
    process.stdout.write("\x1b[?25h");
    process.exit(130);
  };
  process.on("SIGINT", restore);
  await playIntro(logoMeta());
} else {
  console.log(staticLogo(logoMeta()));
}

printCommands(unknown);

// `--intro` plays the logo, shows the help, starts an example agent, then
// launches the inspector so the devtools open with something to show.
if (intro) {
  // Offer to wire the bundled skill into the user's coding agents first.
  await offerSkillInstall();

  // Pick the key here so we can both start under it and point the inspector at
  // it. Matches `start`'s own `agent-<hex>` default for an omitted key.
  const exampleKey = `agent-${randomBytes(2).toString("hex")}`;
  const started = await runStart(exampleKey);
  if (started) {
    // `inspect` reads its args from argv (`process.argv.slice(3)`); rewrite it
    // so the imported command opens directly on the sandbox we just started.
    process.argv = [process.argv[0], process.argv[1], "inspect", exampleKey];
    await import("./inspect/index.js");
  }
} else {
  // Non-zero exit when the user typed something we don't recognize, so the help
  // screen doubles as an error path for scripts.
  process.exit(unknown && !unknown.startsWith("-") ? 1 : 0);
}
