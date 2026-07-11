import {
  streamScreencast,
  type PageMeta,
  type ScreencastFrame,
} from "@/lib/browser";
import { setGatewayUrl } from "@/lib/hiver";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Live browser view. Connects to the task's nested browser VM over CDP and
// relays Page.screencast frames as SSE. Each `data:` is a base64 JPEG frame plus
// its device dimensions (so the client can map click coordinates back).
export async function GET(req: Request): Promise<Response> {
  const url = new URL(req.url);
  const session = url.searchParams.get("session");
  setGatewayUrl(url.searchParams.get("gatewayUrl") ?? undefined);
  if (!session) return new Response("missing session", { status: 400 });

  const encoder = new TextEncoder();

  const stream = new ReadableStream<Uint8Array>({
    async start(controller) {
      const send = (event: string, payload: unknown) => {
        try {
          controller.enqueue(
            encoder.encode(`event: ${event}\ndata: ${JSON.stringify(payload)}\n\n`),
          );
        } catch {
          /* closed */
        }
      };

      const onFrame = (f: ScreencastFrame) =>
        send("frame", {
          data: f.data,
          width: f.metadata.deviceWidth,
          height: f.metadata.deviceHeight,
        });

      const onMeta = (m: PageMeta) => send("meta", m);

      const heartbeat = setInterval(() => {
        try {
          controller.enqueue(encoder.encode(`: ping\n\n`));
        } catch {
          /* closed */
        }
      }, 15000);

      req.signal.addEventListener("abort", () => {
        clearInterval(heartbeat);
        try {
          controller.close();
        } catch {
          /* already closed */
        }
      });

      try {
        const ok = await streamScreencast(session, onFrame, onMeta, req.signal);
        if (!ok) send("unavailable", { reason: "no browser" });
      } catch (err) {
        send("error", { message: err instanceof Error ? err.message : String(err) });
      }
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
