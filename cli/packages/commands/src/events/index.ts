import { listSandboxes, DEFAULT_GATEWAY_URL } from "@hiver.sh/client";
import { brand, accent, bright, bold, dim, red } from "../theme.js";

// Args after the command name (`hiver events <key> ...`).
const args = process.argv.slice(3);
function getArg(name: string): string | undefined {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && i + 1 < args.length ? args[i + 1] : undefined;
}
const key = args.find((a) => !a.startsWith("--"));
const gatewayUrl = getArg("gateway-url") ?? DEFAULT_GATEWAY_URL;

if (!key) {
  console.error(
    `\n  ${red("✖")} missing sandbox key — ${dim("usage: hiver events <sandbox-key>")}\n`,
  );
  process.exit(1);
}

const sandbox = (await listSandboxes({ gatewayUrl })).find(
  (s) => s.key === key,
);
if (!sandbox) {
  console.error(
    `\n  ${red("✖")} no sandbox with key ${bold(key)} on ${dim(gatewayUrl)}\n`,
  );
  process.exit(1);
}

console.log(
  `\n${bold(brand("Events"))} ${accent(sandbox.key)} ${dim("· ctrl-c to stop")}\n`,
);

const ac = new AbortController();
process.on("SIGINT", () => {
  ac.abort();
  console.log(`\n${dim("  stopped.")}\n`);
  process.exit(0);
});

try {
  for await (const event of sandbox.getEventsStream({ signal: ac.signal })) {
    const ts = new Date().toLocaleTimeString();
    console.log(
      `  ${dim(ts)}  ${bright(event.type.padEnd(18))} ${dim(`#${event.id}`)}`,
    );
  }
} catch (err) {
  if (!ac.signal.aborted) {
    console.error(`  ${red("✖")} stream error: ${dim(String(err))}\n`);
    process.exit(1);
  }
}
