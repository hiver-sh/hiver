import { listSandboxes } from "@hiver.sh/client";
import { white, dim } from "../theme.js";
import { createLoader } from "../hive.js";
import { subcommand, withGateway, run, resolveGatewayUrl } from "../args.js";
import { ensureGateway } from "../gateway.js";

const cmd = withGateway(
  subcommand("stop", "Stop a sandbox on the gateway."),
).argument("<key>", "sandbox to stop");
run(cmd);

const key = cmd.args[0];
let gatewayUrl = resolveGatewayUrl(cmd.opts().gatewayUrl);
gatewayUrl = await ensureGateway(gatewayUrl);

console.log();
const loader = createLoader(`Stopping ${dim(key)}`).start();

try {
  const sandbox = (await listSandboxes({ gatewayUrl })).find(
    (s) => s.key === key,
  );
  if (!sandbox) {
    loader.fail(`no sandbox with key ${white(key)} on ${dim(gatewayUrl)}\n`);
    process.exit(1);
  }
  await sandbox.shutdown();
  loader.succeed(`${white(sandbox.key)}  ${dim("stopped")}\n`);
} catch (err) {
  loader.fail(`could not stop sandbox: ${dim(String(err))}\n`);
  process.exit(1);
}
