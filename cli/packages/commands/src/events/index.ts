import { listSandboxes } from "@hiver.sh/client";
import { white, dim, red } from "../theme.js";
import { subcommand, withGateway, run, resolveGatewayUrl } from "../args.js";
import { ensureGateway } from "../gateway.js";
import {
  loadEvents,
  lastOwnEventId,
  appendEvent,
} from "../../../inspector-server/dist/lib/eventStore.js";

const cmd = withGateway(
  subcommand("events", "Stream a sandbox's events live as they happen."),
)
  .argument("<sandbox-key>", "sandbox to stream events from")
  .option("--start-event-id <id>", "start streaming from this event id")
  .option(
    "--jq <filter>",
    "filter/transform each event through a jq expression",
  )
  .option("-f, --follow", "keep streaming and reconnect if the server closes");
run(cmd);
const key = cmd.args[0];
const {
  gatewayUrl: gatewayFlag,
  startEventId,
  jq: jqFilter,
  follow,
} = cmd.opts();
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

// `emit` renders one event to stdout. Without `--jq` that's the raw JSON line;
// with it, each event is run through jq (the real thing, compiled to WASM) as
// its own single-document input — so `select(...)` drops events, `.foo` projects
// a field, and a filter yielding multiple values prints one line each, matching
// how `hiver events <key> | jq <filter>` would behave.
let emit = (obj: unknown) => console.log(JSON.stringify(obj));
if (jqFilter !== undefined) {
  const { loadJq } = await import("jq-wasm");
  const jq = await loadJq();
  // Fail fast on a malformed program. jq-wasm reports a compile/syntax error as
  // exit code 3; probe once against `null` and surface only that. A runtime
  // error there (exit 5) is expected — it says nothing about the real events —
  // so we let it pass.
  const probe = jq.raw(null, jqFilter);
  if (probe.exitCode === 3) {
    console.error(
      `\n  ${red("✖")} invalid jq filter: ${dim(probe.stderr.trim())}\n`,
    );
    process.exit(1);
  }
  emit = (obj: unknown) => {
    const { stdout, stderr, exitCode } = jq.raw(obj as object, jqFilter);
    if (exitCode !== 0) {
      // Per-event runtime error (e.g. the filter hits a field of the wrong type
      // on this event). Report it but keep the stream going.
      console.error(`  ${red("✖")} jq: ${dim(stderr.trim())}`);
      return;
    }
    // jq produces no output for a non-matching `select`; skip those entirely.
    if (stdout !== "") console.log(stdout);
  };
}

const tty = process.stdout.isTTY;
if (tty) console.log();

// Events are persisted by the inspector in SQLite (~/.hiver/events.db). Replay
// what's already stored, then resume the live stream from just after it — so
// this shows full history without re-fetching, and picks up new events without
// gaps or duplicates. `--start-event-id` still wins as an explicit lower bound.
// All SQLite access here is best-effort: a missing or locked DB must never stop
// the stream, so reads fall back to no replay and writes are swallowed.
const flagStart = startEventId !== undefined ? Number(startEventId) : undefined;
let stored: ReturnType<typeof loadEvents> = [];
let resumeId = flagStart;
try {
  stored = loadEvents(sandbox.id, key).filter(
    // loadEvents returns the owner timeline (this sandbox + any nested sandboxes
    // the inspector relayed into it); `hiver events` streams just this sandbox,
    // so replay only its own feed.
    (e) =>
      (e.sandbox_key === undefined || e.sandbox_key === key) &&
      (flagStart === undefined || e.id > flagStart),
  );
  if (stored.length) resumeId = stored[stored.length - 1].id;
  else if (resumeId === undefined) resumeId = lastOwnEventId(sandbox.id, key);
} catch {
  // DB unavailable — skip replay and stream live from --start-event-id (if any).
}

for (const event of stored) {
  // Print the raw event shape (drop the routing fields the inspector adds on
  // storage) so replayed lines match live ones.
  const raw: Record<string, unknown> = { ...event };
  delete raw.sandbox_id;
  delete raw.sandbox_key;
  emit(raw);
}

try {
  for await (const event of sandbox.getEventsStream({
    signal: ac.signal,
    lastEventId: resumeId,
    follow: follow ?? false,
  })) {
    // Persist as we go so a later `hiver events` (or the inspector) sees them.
    try {
      appendEvent(sandbox.id, key, {
        ...event,
        sandbox_id: sandbox.id,
        sandbox_key: key,
      });
    } catch {
      // best-effort — a write failure must not interrupt the stream
    }
    emit(event);
  }
  if (tty) console.log();
} catch (err) {
  if (!ac.signal.aborted) {
    console.error(`  ${red("✖")} stream error: ${dim(String(err))}\n`);
    process.exit(1);
  }
}
