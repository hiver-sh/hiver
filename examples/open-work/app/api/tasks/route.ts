import { publish } from "@/lib/bus";
import {
  AGENT_SESSION_ID,
  ensureWorkspaceDirs,
  getSandbox,
  provisioningFor,
  setGatewayUrl,
  uploadTaskFiles,
} from "@/lib/hiver";
import {
  isOrchestration,
  ORCHESTRATIONS,
  type Orchestration,
} from "@/lib/orchestration";
import { getSession } from "@/lib/session";
import type { ManifestEntry } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Send one message. The response is a small ack — the actual output streams
// over the shared SSE connection (/api/stream), keyed by `sessionId`.
export async function POST(req: Request): Promise<Response> {
  const form = await req.formData();
  setGatewayUrl(String(form.get("gatewayUrl") ?? "") || undefined);
  const sessionId = String(form.get("sessionId") || crypto.randomUUID());
  const prompt = String(form.get("prompt") ?? "").trim();
  const files = form.getAll("file").filter((f): f is File => f instanceof File);
  const orchestrationRaw = String(form.get("orchestration") ?? "claude");
  const orchestration: Orchestration = isOrchestration(orchestrationRaw)
    ? orchestrationRaw
    : "claude";
  const engine = ORCHESTRATIONS[orchestration];
  const apiKey = String(form.get("apiKey") ?? "").trim();
  const ttlSeconds = Number(form.get("ttl")) || undefined;
  const modelRaw = String(form.get("model") ?? "");
  const model = engine.models.some((m) => m.id === modelRaw)
    ? modelRaw
    : engine.defaultModel;

  let manifest: ManifestEntry[] = [];
  try {
    manifest = JSON.parse(String(form.get("manifest") ?? "[]"));
  } catch {
    manifest = [];
  }

  try {
    // No API key → publish a canned reply pointing the user at Settings.
    if (!apiKey) {
      publish(sessionId, { type: "output", text: missingKeyReply(engine.label, engine.env) });
      publish(sessionId, { type: "done" });
      return Response.json({ ok: true });
    }

    // Key is injected via the sandbox's egress override, not the agent env.
    const provisioning = provisioningFor(orchestration, apiKey, sessionId);

    // Provision the sandbox (with the selected ttl) and ensure the input/output
    // convention dirs exist for this turn.
    await ensureWorkspaceDirs(
      await getSandbox(sessionId, engine.image, provisioning, ttlSeconds),
    );

    const buildArgs = (resume: boolean) =>
      engine.sessionArgs(model, { id: AGENT_SESSION_ID, resume });

    // One persistent agent process per task (sandbox keyed by sessionId). Start
    // it — and its fs watcher — BEFORE writing files, so the uploads are seen as
    // fs events and streamed to the client (the input list is event-driven).
    const session = await getSession(
      sessionId,
      engine.image,
      provisioning,
      model,
      buildArgs,
    );

    // Attachments/references are written into /workspace/input; the sandbox
    // emits fs events for those writes, which populate the client's Input list.
    if (files.length > 0) {
      publish(sessionId, { type: "status", message: `Uploading ${files.length} file(s)…` });
      await uploadTaskFiles(sessionId, files, manifest, engine.image, provisioning);
    }

    // If the write fails (process exited), the session is now marked dead, so
    // getSession restarts it — resuming the conversation — and we retry once.
    try {
      await session.send(prompt);
    } catch {
      const fresh = await getSession(sessionId, engine.image, provisioning, model, buildArgs);
      await fresh.send(prompt);
    }

    return Response.json({ ok: true });
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    publish(sessionId, { type: "error", message });
    publish(sessionId, { type: "done" });
    return Response.json({ ok: false, error: message }, { status: 500 });
  }
}

/** Canned markdown reply shown when the engine's API key isn't set. */
function missingKeyReply(engineLabel: string, envName: string): string {
  return [
    `### 🔑 API key required`,
    ``,
    `To run tasks with **${engineLabel}**, add your API key first.`,
    ``,
    `1. Click the **settings** icon (⚙) in the top-right.`,
    `2. Paste your key into the **${engineLabel}** field.`,
    ``,
    `It's injected into the sandbox as \`${envName}\`. Then send your message again.`,
  ].join("\n");
}
