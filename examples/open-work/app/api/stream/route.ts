import { subscribe, type Envelope } from "@/lib/bus";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Single long-lived SSE connection. Every agent event (for any thread) is
// pushed here tagged with its `session` id; the browser routes each to the
// matching task. Sending a message is a separate POST to /api/tasks.
export async function GET(req: Request): Promise<Response> {
  const encoder = new TextEncoder();

  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      const write = (env: Envelope) =>
        controller.enqueue(encoder.encode(`data: ${JSON.stringify(env)}\n\n`));

      const unsubscribe = subscribe(write);
      // Comment heartbeat keeps proxies from closing an idle stream.
      const heartbeat = setInterval(() => {
        try {
          controller.enqueue(encoder.encode(`: ping\n\n`));
        } catch {
          /* closed */
        }
      }, 15000);

      req.signal.addEventListener("abort", () => {
        clearInterval(heartbeat);
        unsubscribe();
        try {
          controller.close();
        } catch {
          /* already closed */
        }
      });
    },
  });

  return new Response(stream, {
    headers: {
      "content-type": "text/event-stream; charset=utf-8",
      "cache-control": "no-cache, no-transform",
      connection: "keep-alive",
    },
  });
}
