import { listSandboxes } from "@hiver.sh/client";
import { accent, dim, red } from "../theme.js";
import { subcommand, withGateway, run, resolveGatewayUrl } from "../args.js";

const cmd = withGateway(
  subcommand("list", "List the sandboxes currently running on the gateway."),
);
run(cmd);
const gatewayUrl = resolveGatewayUrl(cmd.opts().gatewayUrl);

console.log();

try {
  const sandboxes = await listSandboxes({ gatewayUrl });
  if (sandboxes.length === 0) {
    console.log(`${dim("No sandboxes running.")}\n`);
  } else {
    // Fetch each sandbox's exposed ports concurrently; tolerate per-sandbox
    // failures (e.g. one not yet reachable) so the listing still renders.
    const ports = await Promise.all(
      sandboxes.map((s) =>
        s.getPorts({ timeoutMs: 5_000 }).catch(() => [] as number[]),
      ),
    );
    const pad = Math.max(...sandboxes.map((s) => s.key.length));
    sandboxes.forEach((s, i) => {
      const exposed = ports[i].length
        ? ports[i].map((p) => `:${p}`).join(", ")
        : "no ports";
      console.log(`${accent(s.key.padEnd(pad))}  ${dim(exposed)}`);
    });
    console.log(
      `\n${dim(`${sandboxes.length} sandbox${sandboxes.length === 1 ? "" : "es"}`)}\n`,
    );
  }
} catch (err) {
  console.error(
    `${red("✖")} could not reach the gateway: ${dim(String(err))}\n`,
  );
  process.exit(1);
}
