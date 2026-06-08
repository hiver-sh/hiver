import { createInterface } from "node:readline/promises";
import { dim } from "./theme.js";

/** Yes/no prompt. Returns false (the default) without a TTY. */
export async function confirm(question: string): Promise<boolean> {
  if (!process.stdin.isTTY) return false;
  const rl = createInterface({ input: process.stdin, output: process.stdout });
  // Ctrl+C at the prompt: close the readline (which restores stdin out of raw
  // mode) before exiting, so the shell isn't left echoing `^[[A` for arrows.
  rl.on("SIGINT", () => {
    rl.close();
    process.stdout.write("\n");
    process.exit(130);
  });
  try {
    const answer = await rl.question(`${question} ${dim("(y/N)")} `);
    return /^y(es)?$/i.test(answer.trim());
  } catch (err) {
    // Ctrl+C at the prompt rejects with an AbortError — exit cleanly, no trace.
    const e = err as { name?: string; code?: string };
    if (e?.name === "AbortError" || e?.code === "ABORT_ERR") {
      rl.close();
      process.stdout.write("\n");
      process.exit(130);
    }
    throw err;
  } finally {
    rl.close();
  }
}
