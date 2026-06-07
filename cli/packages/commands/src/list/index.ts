import { listSandboxes } from "@hiver.sh/client";
import { brand, accent, bold, dim, red } from "../theme.js";
import { subcommand, withGateway, run, resolveGatewayUrl } from "../args.js";

const cmd = withGateway(
  subcommand("list", "List the sandboxes currently running on the gateway."),
);
run(cmd);
const gatewayUrl = resolveGatewayUrl(cmd.opts().gatewayUrl);

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
