import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { COMMANDS } from "./commands.js";
import { shade, fg, brand, bold, dim, red } from "./theme.js";

// Version from the published package (cli/package.json, three levels up from
// both src/ and dist/).
const __dirname = dirname(fileURLToPath(import.meta.url));
const { version } = JSON.parse(
  readFileSync(resolve(__dirname, "../../../package.json"), "utf8"),
) as { version: string };

/**
 * Default entry point: animate the `hiver` logo, then print the available
 * subcommands.
 *
 * The intro: three ⬢ hexagons (U+2B22) build up and shift through the
 * hive-violet ramp, collapse into one, then the wordmark types in with the
 * version below. Animation only plays on an interactive TTY; pipes, CI, and
 * NO_COLOR get a plain static render.
 */

const HEX = "⬢";
const PAD = "  "; // left margin
const NAME = "Hiver";
const FULL = `${NAME} · Agent Runtime`;

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function hex(intensity: number): string {
  return fg(shade(intensity), HEX);
}

// The wordmark revealed to `n` characters: "Hiver" in bold violet, the rest dim.
function wordmark(n: number): string {
  const visible = FULL.slice(0, n);
  return (
    bold(brand(visible.slice(0, NAME.length))) + dim(visible.slice(NAME.length))
  );
}

// Logo as a single line: hex + wordmark + version, e.g.
// `⬢  Hiver · Agent Runtime v0.1.0`.
function logo(hexPart: string, chars: number, showVersion: boolean): string {
  const word = chars > 0 ? "  " + wordmark(chars) : "";
  const ver = showVersion ? " " + dim(`v${version}`) : "";
  return PAD + hexPart + word + ver;
}

function paint(line: string, first: boolean) {
  // Repaint in place: jump to the top of the line and clear it.
  const prefix = first ? "" : "\x1b[1A";
  process.stdout.write(prefix + "\x1b[0G" + line + "\x1b[K\n");
}

async function animate() {
  process.stdout.write("\x1b[?25l"); // hide cursor
  let first = true;
  const show = (line: string, ms: number) => {
    paint(line, first);
    first = false;
    return sleep(ms);
  };

  // 1 — build up three hexagons, one at a time.
  for (let n = 1; n <= 3; n++) {
    await show(
      logo(Array.from({ length: n }, () => hex(0.9)).join(" "), 0, false),
      140,
    );
  }

  // 2 — pulse them through the violet ramp.
  for (const t of [0.45, 0.85, 0.55, 1, 0.7]) {
    await show(logo([hex(t), hex(t), hex(t)].join(" "), 0, false), 90);
  }

  // 3 — collapse the three into one.
  await show(logo([hex(1), hex(1), hex(1)].join(""), 0, false), 130);
  await show(logo(hex(1), 0, false), 150);
  await show(logo(hex(0.82), 0, false), 110);

  // 4 — type in the wordmark, then the version after it.
  for (let n = 1; n <= FULL.length; n++) {
    await show(logo(hex(0.82), n, false), 22);
  }
  await show(logo(hex(0.82), FULL.length, true), 0);

  process.stdout.write("\x1b[?25h"); // show cursor
}

function staticLogo(): string {
  return logo(hex(0.82), FULL.length, true);
}

function printCommands(unknown: string | undefined) {
  console.log();

  if (unknown && !unknown.startsWith("-")) {
    console.log(`  ${red("✖")} unknown command: ${bold(unknown)}\n`);
  }

  console.log(`  ${dim("Usage:")} hiver ${dim("<command> [options]")}\n`);
  console.log(`  ${bold("Commands")}`);

  const pad = Math.max(...COMMANDS.map((c) => c.name.length));
  for (const cmd of COMMANDS) {
    console.log(`    ${brand(cmd.name.padEnd(pad))}  ${dim(cmd.summary)}`);
  }

  console.log();
  console.log(
    `  ${dim("Run")} hiver ${dim("<command> --help for command details.")}`,
  );
  console.log();
}

const unknown = process.argv[2];
const interactive =
  Boolean(process.stdout.isTTY) && !process.env.CI && !process.env.NO_COLOR;

if (interactive) {
  // Restore the cursor if interrupted mid-animation.
  const restore = () => {
    process.stdout.write("\x1b[?25h");
    process.exit(130);
  };
  process.on("SIGINT", restore);
  await animate();
} else {
  console.log(staticLogo());
}

printCommands(unknown);

// Non-zero exit when the user typed something we don't recognize, so the help
// screen doubles as an error path for scripts.
process.exit(unknown && !unknown.startsWith("-") ? 1 : 0);
