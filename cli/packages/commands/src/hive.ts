import { writeSync } from "node:fs";
import { color, shade, fg, brand, bright, bold, dim, red } from "./theme.js";

// Restore the terminal unconditionally on any exit. writeSync bypasses the
// stream layer so the cursor reset works even as stdout is being torn down.
// We also drop stdin out of raw mode: a readline prompt (e.g. confirm())
// flips stdin to raw, and a Ctrl+C that terminates before the prompt closes
// would otherwise leave the shell in raw mode (arrow keys echo as `^[[A`).
process.on("exit", () => {
  if (process.stdout.isTTY) writeSync(1, "\x1b[?25h");
  if (process.stdin.isTTY && process.stdin.isRaw) {
    try {
      process.stdin.setRawMode(false);
    } catch {
      /* stdin already torn down */
    }
  }
});

/**
 * The hive `⬢` animation, as a reusable component.
 *
 * - `playIntro()` / `staticLogo()` — the one-shot logo shown on the help screen.
 * - `createLoader()` — an ora-like loading indicator that pulses the hexagons
 *   with a travelling violet wave while a task runs.
 *
 * Everything is gated on `color` (NO_COLOR / FORCE_COLOR / TTY) and degrades to
 * plain text when color or a TTY isn't available.
 */

const HEX = "⬢";
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** A single hexagon at the given brightness (0–1) along the violet ramp. */
export function hex(intensity: number): string {
  return fg(shade(intensity), HEX);
}

export interface Loader {
  /** Begin animating (no-op beyond a one-line print when non-interactive). */
  start(): Loader;
  /** Update the trailing label. */
  setText(text: string): void;
  /** Stop and clear the line without a final mark. */
  stop(): void;
  /** Stop and leave a success line. */
  succeed(text?: string): void;
  /** Stop and leave a failure line. */
  fail(text?: string): void;
}

// A single hexagon that alternates orientation — ⬢ (pointy-top) and ⬣
// (flat-top) are the same shape rotated, so cycling them reads as a spin. The
// level sweeps the violet ramp (deep → bright → deep) so it changes colour as
// it turns.
const SPIN: { glyph: string; level: number }[] = [
  { glyph: "⬢", level: 0.3 },
  { glyph: "⬣", level: 0.6 },
  { glyph: "⬢", level: 0.9 },
  { glyph: "⬣", level: 1 },
  { glyph: "⬢", level: 0.9 },
  { glyph: "⬣", level: 0.6 },
];

export function createLoader(label: string): Loader {
  let text = label;
  let frame = 0;
  let timer: ReturnType<typeof setInterval> | undefined;
  const interactive = color && Boolean(process.stdout.isTTY);

  const onSigint = () => {
    process.stdout.write("\x1b[?25h");
    process.exit(130);
  };

  function comb(): string {
    const { glyph, level } = SPIN[frame % SPIN.length];
    return fg(shade(level), glyph);
  }

  function render() {
    process.stdout.write(`\x1b[2K\r${comb()}  ${dim(text)}`);
    frame++;
  }

  function finalize(ok: boolean, msg: string) {
    const mark = ok ? bright("✔") : red("✖");
    // Always close with a single trailing blank line so output after the loader
    // is separated; strip any newline the caller added so it never doubles up.
    process.stdout.write(`${mark} ${msg.replace(/\n+$/, "")}\n\n`);
  }

  return {
    start() {
      if (!interactive) {
        process.stdout.write(`${dim(text + "…")}\n`);
        return this;
      }
      process.stdout.write("\x1b[?25l"); // hide cursor
      process.once("SIGINT", onSigint);
      render();
      timer = setInterval(render, 120);
      return this;
    },
    setText(next) {
      text = next;
    },
    stop() {
      if (timer) clearInterval(timer);
      timer = undefined;
      if (interactive) {
        process.stdout.write("\x1b[2K\r\x1b[?25h");
        process.off("SIGINT", onSigint);
      }
    },
    succeed(msg) {
      this.stop();
      finalize(true, msg ?? text);
    },
    fail(msg) {
      this.stop();
      finalize(false, msg ?? text);
    },
  };
}

/** Branding for the logo intro; supplied by the caller (see index.ts). */
export interface HiveLogo {
  /** Wordmark name, shown bold (e.g. "Hiver"). */
  name: string;
  /** Tagline after the name (e.g. "Agent Runtime"). */
  tagline: string;
  /** Version string, appended dim (e.g. "0.1.0"). */
  version: string;
}

// The wordmark revealed to `n` characters: the name in bold violet, the rest dim.
function wordmark(meta: HiveLogo, n: number): string {
  const full = `${meta.name} · ${meta.tagline}`;
  const visible = full.slice(0, n);
  return (
    bold(brand(visible.slice(0, meta.name.length))) +
    dim(visible.slice(meta.name.length))
  );
}

// Logo as a single line: hex + wordmark + version.
function logo(
  meta: HiveLogo,
  hexPart: string,
  chars: number,
  showVersion: boolean,
): string {
  const word = chars > 0 ? " " + wordmark(meta, chars) : "";
  const ver = showVersion ? " " + dim(`v${meta.version}`) : "";
  return hexPart + word + ver;
}

function paint(line: string, first: boolean) {
  // Repaint in place: jump to the top of the line and clear it.
  const prefix = first ? "" : "\x1b[1A";
  process.stdout.write(prefix + "\x1b[0G" + line + "\x1b[K\n");
}

/** The one-shot intro: build up three hexagons, collapse to one, type the wordmark. */
export async function playIntro(meta: HiveLogo) {
  const fullLen = `${meta.name} · ${meta.tagline}`.length;
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
      logo(meta, Array.from({ length: n }, () => hex(0.9)).join(" "), 0, false),
      140,
    );
  }

  // 2 — pulse them through the violet ramp.
  for (const t of [0.45, 0.85, 0.55, 1, 0.7]) {
    await show(logo(meta, [hex(t), hex(t), hex(t)].join(" "), 0, false), 90);
  }

  // 3 — collapse the three into one.
  await show(logo(meta, [hex(1), hex(1), hex(1)].join(""), 0, false), 130);
  await show(logo(meta, hex(1), 0, false), 150);
  await show(logo(meta, hex(0.82), 0, false), 110);

  // 4 — type in the wordmark, then the version after it.
  for (let n = 1; n <= fullLen; n++) {
    await show(logo(meta, hex(0.82), n, false), 22);
  }
  await show(logo(meta, hex(0.82), fullLen, true), 0);

  process.stdout.write("\x1b[?25h"); // show cursor
}

/** The resting logo, for pipes / CI / NO_COLOR. */
export function staticLogo(meta: HiveLogo): string {
  const fullLen = `${meta.name} · ${meta.tagline}`.length;
  return logo(meta, hex(0.82), fullLen, true);
}
