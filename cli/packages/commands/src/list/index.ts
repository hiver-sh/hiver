import { listSandboxes, DEFAULT_GATEWAY_URL } from "@hiver.sh/client";
import { brand, accent, bold, dim, red } from "../theme.js";

// Args after the command name (`hiver list ...`).
const args = process.argv.slice(3);
function getArg(name: string): string | undefined {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && i + 1 < args.length ? args[i + 1] : undefined;
}
const gatewayUrl = getArg("gateway-url") ?? DEFAULT_GATEWAY_URL;

console.log(`\n${bold(brand("Sandboxes"))} ${dim(gatewayUrl)}\n`);

try {
  const sandboxes = await listSandboxes({ gatewayUrl });
  if (sandboxes.length === 0) {
    console.log(`  ${dim("No sandboxes running.")}\n`);
  } else {
    const pad = Math.max(...sandboxes.map((s) => s.key.length));
    for (const s of sandboxes) {
      console.log(`  ${accent(s.key.padEnd(pad))}  ${dim(s.id)}`);
    }
    console.log(
      `\n  ${dim(`${sandboxes.length} sandbox${sandboxes.length === 1 ? "" : "es"}`)}\n`,
    );
  }
} catch (err) {
  console.error(
    `  ${red("✖")} could not reach the gateway: ${dim(String(err))}\n`,
  );
  process.exit(1);
}
