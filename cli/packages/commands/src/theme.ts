import pc from "picocolors";

/**
 * Shared hive-violet theme for the CLI. Matches the hive site's brand purples
 * (#8b5cf6 / #7c3aed / #c4b5fd, indigo accent #818cf8).
 *
 * Truecolor escapes are gated on picocolors' detection (NO_COLOR / FORCE_COLOR
 * / TTY) so output degrades to plain text when color isn't supported.
 */
export const color = pc.isColorSupported;

type Rgb = [number, number, number];

// Brand ramp, dark → bright. Intensity in [0,1] lerps across the stops.
export const VIOLET: Rgb[] = [
  [46, 16, 80],
  [91, 33, 182],
  [124, 58, 237],
  [139, 92, 246],
  [196, 181, 253],
];

export function shade(intensity: number): Rgb {
  const t = Math.max(0, Math.min(1, intensity)) * (VIOLET.length - 1);
  const i = Math.floor(t);
  const f = t - i;
  const a = VIOLET[i];
  const b = VIOLET[Math.min(VIOLET.length - 1, i + 1)];
  return [
    Math.round(a[0] + (b[0] - a[0]) * f),
    Math.round(a[1] + (b[1] - a[1]) * f),
    Math.round(a[2] + (b[2] - a[2]) * f),
  ];
}

export function fg([r, g, b]: Rgb, s: string): string {
  return color ? `\x1b[38;2;${r};${g};${b}m${s}\x1b[0m` : s;
}

const tone = (rgb: Rgb) => (s: string) => fg(rgb, s);

/** Mid brand violet (#8b5cf6) — headings, command names. */
export const brand = tone([139, 92, 246]);
/** Bright lavender (#c4b5fd) — success / highlights. */
export const bright = tone([196, 181, 253]);
/** Indigo accent (#818cf8) — secondary stream, distinct from `brand`. */
export const accent = tone([129, 140, 248]);

export const bold = pc.bold;
export const dim = pc.dim;
export const red = pc.red;

/** `[label]` tag in the given tone, e.g. `tag("server", brand)`. */
export function tag(label: string, tone: (s: string) => string): string {
  return tone(`[${label}]`) + " ";
}
