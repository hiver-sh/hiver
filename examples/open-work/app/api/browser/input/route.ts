import { dispatchInput, type BrowserInput } from "@/lib/browser";
import { setGatewayUrl } from "@/lib/hiver";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Viewer input (mouse/keyboard) → CDP Input events on the task's browser VM.
export async function POST(req: Request): Promise<Response> {
  const body = (await req.json()) as {
    session?: string;
    gatewayUrl?: string;
    event?: BrowserInput;
  };
  setGatewayUrl(body.gatewayUrl);
  if (!body.session || !body.event) {
    return new Response("missing session or event", { status: 400 });
  }

  try {
    const result = await dispatchInput(body.session, body.event);
    return Response.json(result);
  } catch (err) {
    return Response.json(
      { ok: false, error: err instanceof Error ? err.message : String(err) },
      { status: 502 },
    );
  }
}
