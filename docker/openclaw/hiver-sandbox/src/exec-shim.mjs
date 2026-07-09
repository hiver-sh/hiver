// Local child process that streams one exec into a Hiver sandbox.
//
// OpenClaw's exec runtime spawns this via the argv returned from the backend's
// buildExecSpec, then pipes the model's stdin to us and captures our stdout/
// stderr. We reconnect to the sandbox by key, run the command through Hiver's
// streaming exec, forward stdin, relay output, and exit with the command's code.
//
// All inputs arrive as environment variables (set in buildExecSpec) so the
// arbitrary shell command never has to survive argv quoting.

import { getOrCreateSandbox } from "@hiver.sh/client";

async function main() {
  // buildExecSpec always sets HIVER_GATEWAY_URL to the resolved (non-empty) URL;
  // `|| undefined` keeps an empty value from producing an invalid URL.
  const gatewayUrl = process.env.HIVER_GATEWAY_URL || undefined;
  const key = process.env.HIVER_SANDBOX_KEY;
  // Must match the image the backend factory created this key with. The gateway
  // routes get-or-create onto a pack host by the x-hiver-image header, so an
  // empty/wrong image lands on a different cluster where the key doesn't exist
  // and a SECOND sandbox (defaulting to agent-base) gets spun up instead of
  // reusing the one exec should run in.
  const image = process.env.HIVER_SANDBOX_IMAGE || undefined;
  const command = process.env.HIVER_EXEC_COMMAND ?? "";
  const cwd = process.env.HIVER_EXEC_WORKDIR || undefined;
  const tty = process.env.HIVER_EXEC_TTY === "1";

  let env;
  try {
    env = JSON.parse(process.env.HIVER_EXEC_ENV || "{}");
  } catch {
    env = {};
  }

  if (!key) {
    process.stderr.write("hiver exec-shim: missing HIVER_SANDBOX_KEY\n");
    process.exit(127);
    return;
  }

  // The sandbox already exists (the backend provisioned it); passing the same
  // image routes to the same pod and resolves the existing handle by key.
  const sandbox = await getOrCreateSandbox(
    key,
    image ? { image } : {},
    { gatewayUrl },
  );

  const proc = await sandbox.execStream(command, {
    cwd,
    env,
    tty,
  });

  // Forward the model's stdin into the sandbox process.
  process.stdin.on("data", (chunk) => {
    proc.writeStdin(chunk.toString("utf8")).catch(() => {
      /* process may have already exited; ignore */
    });
  });
  process.stdin.on("error", () => {
    /* stdin closed early; nothing to forward */
  });

  // Relay sandbox output to our stdio so the supervisor captures it. Await this
  // drain BEFORE exiting: `proc.exitCode` resolves the moment the exit frame is
  // read, which can race ahead of earlier stdout frames still queued in `pipes`.
  // Exiting on exitCode alone would truncate that output (the empty-output bug).
  const drain = (async () => {
    for await (const frame of proc.pipes) {
      if (frame.stdout !== undefined) process.stdout.write(frame.stdout);
      if (frame.stderr !== undefined) process.stderr.write(frame.stderr);
    }
  })();

  const [code] = await Promise.all([
    proc.exitCode,
    drain.catch((err) => {
      process.stderr.write(`hiver exec-shim: stream error: ${String(err)}\n`);
    }),
  ]);

  // All output has been written; exit explicitly (an idle undici socket could
  // otherwise keep the loop alive and hang the exec) but only once the stdout
  // pipe has flushed so nothing is truncated.
  const exit = () => process.exit(typeof code === "number" ? code : 1);
  if (process.stdout.writableLength === 0) exit();
  else process.stdout.once("drain", exit);
}

main().catch((err) => {
  process.stderr.write(`hiver exec-shim: ${String(err?.stack || err)}\n`);
  process.exit(1);
});
