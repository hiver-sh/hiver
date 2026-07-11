import { newTaskId, shutdownSandbox } from "@/lib/hiver";
import { stopSession } from "@/lib/session";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Create a task: the server mints a unique short id, which becomes the task's
// sandbox key. The client then navigates to /task/<id>.
export async function POST(): Promise<Response> {
  return Response.json({ id: newTaskId() });
}

// Delete a task: terminate its agent process and shut down its sandbox.
export async function DELETE(req: Request): Promise<Response> {
  const key = new URL(req.url).searchParams.get("key");
  if (!key) return Response.json({ ok: false, error: "Missing key" }, { status: 400 });
  stopSession(key);
  await shutdownSandbox(key);
  return Response.json({ ok: true });
}
