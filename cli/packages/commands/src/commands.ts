/**
 * Registry of every subcommand exposed by the CLI.
 *
 * `index.ts` reads this to render the help screen, and `bin.js` reads it
 * (indirectly, by name) to route argv to the matching entry module.
 */
export interface CommandSpec {
  /** Name typed on the command line, e.g. `hiver inspect`. */
  name: string;
  /** One-line summary shown in the help listing. */
  summary: string;
  /** Entry module (relative to `src/`) that implements the command. */
  entry: string;
  /** Optional usage examples shown beneath the listing. */
  usage?: string[];
}

export const COMMANDS: CommandSpec[] = [
  {
    name: "up",
    summary: "Start the stack.",
    entry: "compose/index.ts",
    usage: ["hiver up"],
  },
  {
    name: "down",
    summary: "Stop the stack.",
    entry: "compose/index.ts",
    usage: ["hiver down"],
  },
  {
    name: "list",
    summary: "List the sandboxes currently running on the gateway.",
    entry: "list/index.ts",
    usage: ["hiver list", "hiver list --gateway-url <url>"],
  },
  {
    name: "events",
    summary: "Stream a sandbox's events live as they happen.",
    entry: "events/index.ts",
    usage: [
      "hiver events <sandbox-key>",
      "hiver events <sandbox-key> --gateway-url <url>",
    ],
  },
  {
    name: "inspect",
    summary: "Launch the Hiver DevTools.",
    entry: "inspect/index.ts",
    usage: [
      "hiver inspect",
      "hiver inspect --record",
      "hiver inspect --record --gateway-url <url>",
    ],
  },
  {
    name: "bundle",
    summary: "Bundle a Docker image into a Hiver runtime image.",
    entry: "bundle/index.ts",
    usage: ["hiver bundle <image>", "hiver bundle <image> --tag <runtime-tag>"],
  },
];

export function findCommand(name: string | undefined): CommandSpec | undefined {
  return COMMANDS.find((c) => c.name === name);
}
