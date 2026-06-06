import { findCommand } from "./commands.js";

/**
 * Entry router. Looks up argv[2] in the command registry and hands off to the
 * matching module; anything unrecognized falls through to the help listing in
 * `index.ts`. Command modules run via their top-level side effects on import.
 */
const command = findCommand(process.argv[2]);

if (command) {
  await import(`./${command.entry.replace(/\.ts$/, ".js")}`);
} else {
  await import("./index.js");
}
