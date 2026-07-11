import { createInterface } from "node:readline/promises";
import { emitKeypressEvents } from "node:readline";
import { brand, bright, dim } from "./theme.js";

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

export interface SelectOption<T extends string> {
  label: string;
  value: T;
}

/**
 * Arrow-key selection menu. Returns the selected value.
 * Falls back to returning the first option when stdin is not a TTY.
 */
export async function select<T extends string>(
  question: string,
  options: SelectOption<T>[],
): Promise<T> {
  if (!process.stdin.isTTY) return options[0].value;

  return new Promise((resolve) => {
    let idx = 0;

    const render = (first: boolean) => {
      if (!first) process.stdout.write(`\x1b[${options.length + 1}A`);
      process.stdout.write(`\x1b[2K${question}\n`);
      for (let i = 0; i < options.length; i++) {
        const active = i === idx;
        const cursor = active ? brand("›") : " ";
        const label = active ? bright(options[i].label) : dim(options[i].label);
        process.stdout.write(`\x1b[2K  ${cursor} ${label}\n`);
      }
    };

    // Blank line above the menu, printed once so the redraw (which only rewinds
    // over the question + options) leaves it in place.
    process.stdout.write("\n");
    render(true);

    emitKeypressEvents(process.stdin);
    process.stdin.setRawMode(true);
    process.stdout.write("\x1b[?25l");

    const onKeypress = (
      _: unknown,
      key: { name: string; ctrl: boolean } | undefined,
    ) => {
      if (!key) return;
      if (key.ctrl && key.name === "c") {
        cleanup();
        process.stdout.write("\n");
        process.exit(130);
      }
      if (key.name === "up") idx = (idx - 1 + options.length) % options.length;
      else if (key.name === "down") idx = (idx + 1) % options.length;
      else if (key.name === "return") {
        cleanup();
        resolve(options[idx].value);
        return;
      }
      render(false);
    };

    const cleanup = () => {
      process.stdin.removeListener("keypress", onKeypress);
      process.stdin.setRawMode(false);
      process.stdout.write("\x1b[?25h");
    };

    process.stdin.on("keypress", onKeypress);
  });
}
