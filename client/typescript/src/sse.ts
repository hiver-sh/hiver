export interface SSEFrame {
  /** Concatenated `data:` payload for this event. */
  data: string;
  /** Last `id:` field seen up to and including this frame, if any. */
  lastEventId?: string;
}

export async function* parseSSE(
  body: ReadableStream<Uint8Array>,
  signal?: AbortSignal,
): AsyncGenerator<SSEFrame, void, void> {
  const reader = body.getReader();
  const decoder = new TextDecoder("utf-8");
  let buffer = "";
  let lastEventId: string | undefined;

  const onAbort = () => {
    // cancel() rejects pending reads with the signal's reason, which
    // unwinds the generator via the for-await caller.
    reader.cancel(signal?.reason).catch(() => {});
  };
  signal?.addEventListener("abort", onAbort, { once: true });

  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });

      // Normalize line endings so a single split handles either CRLF or LF.
      buffer = buffer.replace(/\r\n/g, "\n");

      let sep: number;
      while ((sep = buffer.indexOf("\n\n")) !== -1) {
        const frame = buffer.slice(0, sep);
        buffer = buffer.slice(sep + 2);
        const data: string[] = [];
        for (const raw of frame.split("\n")) {
          if (raw === "" || raw.startsWith(":")) continue;
          const colon = raw.indexOf(":");
          const field = colon === -1 ? raw : raw.slice(0, colon);
          let value = colon === -1 ? "" : raw.slice(colon + 1);
          if (value.startsWith(" ")) value = value.slice(1);
          if (field === "data") data.push(value);
          else if (field === "id") lastEventId = value;
        }
        if (data.length === 0) continue;
        yield { data: data.join("\n"), lastEventId };
      }
    }
  } finally {
    signal?.removeEventListener("abort", onAbort);
    reader.releaseLock();
  }
}
