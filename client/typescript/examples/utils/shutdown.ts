import process from "node:process";
import * as hiver from "@hiver.sh/client";

export function createShutdown(
  sandbox: hiver.Sandbox,
  opts?: {
    /** Extra teardown to run before the sandbox is stopped. */
    cleanup?: () => void | Promise<void>;
  },
): { ac: AbortController; shutdown: (code?: number) => Promise<void> } {
  const ac = new AbortController();
  let promise: Promise<void> | undefined;

  function shutdown(code = 0): Promise<void> {
    if (!promise) {
      promise = (async () => {
        ac.abort();
        await opts?.cleanup?.();
        await sandbox.shutdown().catch(() => {});
      })().finally(() => process.exit(code));
    }
    return promise;
  }

  process.once("SIGINT", () => shutdown(130));
  process.once("SIGTERM", () => shutdown(143));

  return { ac, shutdown };
}
