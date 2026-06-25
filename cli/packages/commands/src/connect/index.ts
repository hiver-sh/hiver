import { white, dim, brand } from "../theme.js";
import { subcommand, run } from "../args.js";
import { writeConfig, CONFIG_PATH } from "../config.js";

const cmd = subcommand(
  "connect",
  "Set the default gateway URL used by all commands.",
).argument("<url>", "gateway URL, e.g. http://gateway");
run(cmd);

const url = cmd.args[0];

// Reject anything that isn't a usable http(s) URL before we persist it — a
// bad value here would silently break every other command's default.
let parsed: URL;
try {
  parsed = new URL(url);
} catch {
  console.error(`\n  ${dim("not a valid URL:")} ${white(url)}\n`);
  process.exit(1);
}
if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
  console.error(
    `\n  ${dim("gateway URL must be http or https:")} ${white(url)}\n`,
  );
  process.exit(1);
}

writeConfig({ gatewayUrl: url });

console.log(
  `\n  ${brand("✔")} gateway set to ${white(url)}  ${dim(CONFIG_PATH)}\n`,
);
