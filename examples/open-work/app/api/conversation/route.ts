import { probeBrowser } from "@/lib/browser";
import { publish } from "@/lib/bus";
import { readConversation, setGatewayUrl } from "@/lib/hiver";
import type { ConversationMessage } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Rebuild a thread from its persisted transcript in the sandbox, so a browser
// refresh doesn't lose the conversation. Returns an array of role-tagged
// messages ({ role, content }). Engine-agnostic: `readConversation` reads either
// the Claude transcript or the codex rollout, whichever the sandbox has.
export async function GET(req: Request): Promise<Response> {
  const url = new URL(req.url);
  setGatewayUrl(url.searchParams.get("gatewayUrl") ?? undefined);
  const session = url.searchParams.get("session");
  if (!session) return Response.json({ turns: [] });

  let turns: ConversationMessage[] = [];
  try {
    turns = await readConversation(session);
  } catch {
    /* no transcript yet */
  }

  // A refresh drops the client's in-memory browser flag. In the background
  // (non-blocking) ask Hiver whether this task's browser VM exists and dial its
  // CDP socket (1s timeout); if it answers, push a browser event over the SSE so
  // the client restores the viewer.
  void probeBrowser(session, () =>
    publish(session, { type: "browser", ready: true }),
  ).catch(() => {});

  return Response.json({ turns });
}
