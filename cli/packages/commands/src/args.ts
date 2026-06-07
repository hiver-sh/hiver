import { Command, Help } from "commander";
import { DEFAULT_GATEWAY_URL } from "@hiver.sh/client";
import { brand, gray } from "./theme.js";
import { readConfig } from "./config.js";

/**
 * A commander parser for a subcommand. Registering options here gives each
 * command a `--help` that lists its own flags. Build it up with `.option(...)`
 * / `.argument(...)`, then call `run()`.
 */
export function subcommand(name: string, description: string): Command {
  const cmd = new Command()
    .name(`hiver ${name}`)
    .description(description)
    .helpOption("-h, --help", "show this help");
  // Theme the usage line (`hiver <name> [options]`) violet. Only this line is
  // coloured — commander measures the aligned columns by raw string length, so
  // colouring option terms would break their padding.
  cmd.configureHelp({
    commandUsage(this: Help, c: Command) {
      return brand(Help.prototype.commandUsage.call(this, c));
    },
    optionDescription(option) {
      return gray(Help.prototype.optionDescription.call(this, option));
    },
  });
  return cmd;
}

/** Add the shared `--gateway-url` option. */
export function withGateway(cmd: Command): Command {
  return cmd.option(
    "--gateway-url <url>",
    "gateway URL (default: saved by `hiver up`, else http://localhost:10000)",
  );
}

/** Parse the args that follow the command name (`hiver <name> …`). */
export function run(cmd: Command): Command {
  return cmd.parse(process.argv.slice(3), { from: "user" });
}

/** Resolve the gateway URL: `--gateway-url` flag → saved config → default. */
export function resolveGatewayUrl(flag?: string): string {
  return flag ?? readConfig().gatewayUrl ?? DEFAULT_GATEWAY_URL;
}
