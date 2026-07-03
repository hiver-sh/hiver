// Reusable exponential-backoff retry, shared by anything that has to poll a
// sandbox resource that only becomes available asynchronously (the CDP relay,
// exposed ports, a booting server, ...). Keeping this generic means callers
// describe *what* "ready" means and leave the timing policy here.

export interface BackoffOptions {
  // Bail out early when this fires (e.g. the SSE client disconnected while we
  // were still polling). Aborting resolves the retry as "gave up", not an error.
  signal?: AbortSignal;
  // Total wall-clock budget across all attempts; once exceeded we stop and
  // report failure rather than retrying forever.
  timeoutMs?: number;
  // Delay before the second attempt. Grows by `factor` after each miss, capped
  // at `maxDelayMs`.
  initialDelayMs?: number;
  // Upper bound on any single inter-attempt delay.
  maxDelayMs?: number;
  // Growth factor applied to the delay after every failed attempt.
  factor?: number;
}

const DEFAULTS = {
  timeoutMs: 30_000,
  initialDelayMs: 250,
  maxDelayMs: 3_000,
  factor: 2,
} satisfies Required<Omit<BackoffOptions, "signal">>;

// Resolve after `ms`, or reject with an "aborted" error if the signal fires
// first. Cleans up its own timer and listener so nothing leaks when either
// side wins.
export function delay(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new Error("aborted"));
      return;
    }
    const onAbort = () => {
      clearTimeout(timer);
      reject(new Error("aborted"));
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

// Run `attempt` repeatedly until it yields a non-null result, backing off
// exponentially between tries. Returns that result, or null once the time
// budget is spent or the signal aborts.
//
// `attempt` returning null/undefined means "not ready yet, try again"; throwing
// is treated the same way (the error is swallowed) so a transient probe failure
// doesn't abort the whole loop. Anything non-null is a success and is returned
// immediately.
export async function retryWithBackoff<T>(
  attempt: (ctx: { attempt: number }) => Promise<T | null | undefined>,
  options: BackoffOptions = {},
): Promise<T | null> {
  const { signal, timeoutMs, initialDelayMs, maxDelayMs, factor } = {
    ...DEFAULTS,
    ...options,
  };
  const deadline = Date.now() + timeoutMs;
  let delayMs = initialDelayMs;

  for (let attemptNo = 1; ; attemptNo++) {
    if (signal?.aborted) return null;
    try {
      const result = await attempt({ attempt: attemptNo });
      if (result != null) return result;
    } catch {
      // Treat a thrown attempt as "not ready" and fall through to the wait.
    }

    // Don't sleep past the deadline: if there's no time left for another real
    // attempt, give up now instead of waiting only to time out.
    const remaining = deadline - Date.now();
    if (remaining <= 0) return null;

    try {
      await delay(Math.min(delayMs, remaining), signal);
    } catch {
      return null; // aborted mid-wait
    }
    delayMs = Math.min(delayMs * factor, maxDelayMs);
  }
}
