import { listSandboxes } from "@hiver.sh/client";
import { white, dim, red } from "../theme.js";
import { subcommand, withGateway, run, resolveGatewayUrl } from "../args.js";
import { ensureGateway } from "../gateway.js";

const cmd = withGateway(
  subcommand("events", "Stream a sandbox's events live as they happen."),
)
  .argument("<sandbox-key>", "sandbox to stream events from")
  .option("--start-event-id <id>", "start streaming from this event id")
  .option("-f, --follow", "keep streaming and reconnect if the server closes");
run(cmd);
const key = cmd.args[0];
const { gatewayUrl: gatewayFlag, startEventId, follow } = cmd.opts();
let gatewayUrl = resolveGatewayUrl(gatewayFlag);
gatewayUrl = await ensureGateway(gatewayUrl);

const sandbox = (await listSandboxes({ gatewayUrl })).find(
  (s) => s.key === key,
);
if (!sandbox) {
  console.error(
    `\n  ${red("✖")} no sandbox with key ${white(key)} on ${dim(gatewayUrl)}\n`,
  );
  process.exit(1);
}

process.stdout.on("error", (err: NodeJS.ErrnoException) => {
  if (err.code === "EPIPE") process.exit(0);
});

const ac = new AbortController();
process.on("SIGINT", () => {
  ac.abort();
  process.exit(0);
});

const tty = process.stdout.isTTY;
if (tty) console.log();
try {
  for await (const event of sandbox.getEventsStream({
    signal: ac.signal,
    lastEventId: startEventId !== undefined ? Number(startEventId) : undefined,
    follow: follow ?? false,
  })) {
    console.log(JSON.stringify(event));
  }
  if (tty) console.log();
} catch (err) {
  if (!ac.signal.aborted) {
    console.error(`  ${red("✖")} stream error: ${dim(String(err))}\n`);
    process.exit(1);
  }
}
