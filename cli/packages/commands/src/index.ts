import { spawn } from "node:child_process";
import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { COMMANDS } from "./commands.js";
import { HIVER_DIR } from "./config.js";
import { bold, dim, red, white } from "./theme.js";
import { playIntro, staticLogo, type HiveLogo } from "./hive.js";

// The `hiver` entry, for spawning subcommands (e.g. `start`). One level up from
// both src/ and dist/.
const BIN = resolve(dirname(fileURLToPath(import.meta.url)), "../bin.js");

// Start an example agent (the CLI's own `start`), inheriting stdio so its
// output shows. Used on the intro/first-run path so the inspector opens with a
// running sandbox to look at. Best-effort: failure shouldn't block the intro.
function runStart(): Promise<boolean> {
  return new Promise((res) => {
    const child = spawn(process.execPath, [BIN, "start"], { stdio: "inherit" });
    child.on("error", () => res(false));
    child.on("exit", (code) => res(code === 0));
  });
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
const intro = introFlag || firstRun;
const interactive =
  Boolean(process.stdout.isTTY) && !process.env.CI && !process.env.NO_COLOR;

console.log();
if (interactive) {
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
  await runStart();
  await import("./inspect/index.js");
} else {
  // Non-zero exit when the user typed something we don't recognize, so the help
  // screen doubles as an error path for scripts.
  process.exit(unknown && !unknown.startsWith("-") ? 1 : 0);
}
