import { listWorkspaceFiles, setGatewayUrl } from "@/lib/hiver";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// List a task's input/output files from its sandbox. Used on refresh to restore
// the Workspace panel, since the live fs events are only emitted once.
export async function GET(req: Request): Promise<Response> {
  const url = new URL(req.url);
  setGatewayUrl(url.searchParams.get("gatewayUrl") ?? undefined);
  const session = url.searchParams.get("session");
  if (!session) return Response.json({ inputs: [], outputs: [] });
  try {
    return Response.json(await listWorkspaceFiles(session));
  } catch {
    return Response.json({ inputs: [], outputs: [] });
  }
}
