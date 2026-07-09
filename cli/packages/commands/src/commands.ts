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
    summary: "Bring up local stack",
    entry: "compose/index.ts",
    usage: ["hiver up"],
  },
  {
    name: "down",
    summary: "Bring down local stack",
    entry: "compose/index.ts",
    usage: ["hiver down"],
  },
  {
    name: "connect",
    summary: "Connect to remote stack",
    entry: "connect/index.ts",
    usage: ["hiver connect <url>", "hiver connect http://gateway"],
  },
  {
    name: "start",
    summary: "Start a sandbox",
    entry: "start/index.ts",
    usage: [
      "hiver start",
      "hiver start <sandbox-key>",
      "hiver start --image <image> --entrypoint <cmd> --ttl <seconds>",
    ],
  },
  {
    name: "run",
    summary: "Build and launch a project directory as a sandbox",
    entry: "run/index.ts",
    usage: ["hiver run", "hiver run <dir>", "hiver run <dir> <key>"],
  },
  {
    name: "stop",
    summary: "Stop a sandbox",
    entry: "stop/index.ts",
    usage: ["hiver stop <sandbox-key>"],
  },
  {
    name: "shell",
    summary: "Open an interactive shell in a sandbox",
    entry: "shell/index.ts",
    usage: [
      "hiver shell <sandbox-key>",
      "hiver shell <sandbox-key> --command /bin/bash",
    ],
  },
  {
    name: "list",
    summary: "List the sandboxes",
    entry: "list/index.ts",
    usage: ["hiver list", "hiver list --gateway-url <url>"],
  },
  {
    name: "events",
    summary: "Stream a sandbox's events live as they happen",
    entry: "events/index.ts",
    usage: [
      "hiver events <sandbox-key>",
      "hiver events <sandbox-key> --gateway-url <url>",
    ],
  },
  {
    name: "inspect",
    summary: "Launch the inspector",
    entry: "inspect/index.ts",
    usage: [
      "hiver inspect",
      "hiver inspect --record",
      "hiver inspect --record --gateway-url <url>",
    ],
  },
  {
    name: "bundle",
    summary: "Bundle a Docker image into a Hiver runtime image",
    entry: "bundle/index.ts",
    usage: [
      "hiver bundle <image>",
      "hiver bundle <image> --tag <runtime-tag>",
      "hiver bundle <image> --tag <runtime-tag> --platform linux/amd64,linux/arm64",
    ],
  },
];

export function findCommand(name: string | undefined): CommandSpec | undefined {
  return COMMANDS.find((c) => c.name === name);
}
