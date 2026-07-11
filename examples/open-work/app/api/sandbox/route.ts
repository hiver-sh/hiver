import { reloadBrowser } from "@/lib/browser";
import {
  allowEgress,
  currentGatewayUrl,
  currentSandboxImage,
  getSandbox,
  provisioningFor,
  setGatewayUrl,
} from "@/lib/hiver";
import { isOrchestration, ORCHESTRATIONS } from "@/lib/orchestration";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

const msg = (err: unknown) => (err instanceof Error ? err.message : String(err));

/** Report a task sandbox's reachability and basic facts. `?key=<taskId>`. */
export async function GET(req: Request): Promise<Response> {
  const url = new URL(req.url);
  setGatewayUrl(url.searchParams.get("gatewayUrl") ?? undefined);
  const key = url.searchParams.get("key");
  if (!key) return Response.json({ online: false, error: "No active task." });

  try {
    const sandbox = await getSandbox(key);
    const [info, ports] = await Promise.all([
      sandbox.getInfo(),
      sandbox.getPorts().catch(() => [] as number[]),
    ]);
    return Response.json({
      key,
      image: currentSandboxImage(key),
      online: true,
      id: sandbox.id,
      isolation: info.isolation,
      ports,
      inspectorUrl: inspectorUrl(sandbox.id, sandbox.key),
    });
  } catch (err) {
    return Response.json({
      key,
      image: currentSandboxImage(key),
      online: false,
      error: msg(err),
    });
  }
}

/**
 * `{ action: "ping", key }` times a round trip. `{ action: "switch", key,
 * orchestration }` reprovisions that task's sandbox with the engine's image.
 */
export async function POST(req: Request): Promise<Response> {
  let body: {
    action?: string;
    key?: string;
    orchestration?: string;
    apiKey?: string;
    ttl?: number;
    host?: string;
    gatewayUrl?: string;
  } = {};
  try {
    body = await req.json();
  } catch {
    /* no body → default ping */
  }
  setGatewayUrl(body.gatewayUrl);
  const key = body.key;
  if (!key) return Response.json({ online: false, error: "No active task." });

  try {
    if (body.action === "switch") {
      const raw = body.orchestration ?? "";
      const orchestration = isOrchestration(raw) ? raw : "claude";
      const engine = ORCHESTRATIONS[orchestration];
      const prov = provisioningFor(orchestration, body.apiKey ?? "", key);
      const sandbox = await getSandbox(key, engine.image, prov);
      return Response.json({ online: true, id: sandbox.id, image: engine.image });
    }

    if (body.action === "ttl") {
      // Apply the new TTL (seconds) to the running sandbox via applyConfig.
      await getSandbox(key, undefined, undefined, Math.max(0, body.ttl ?? 0));
      return Response.json({ online: true });
    }

    if (body.action === "allow-egress") {
      if (body.host) {
        await allowEgress(key, body.host);
        // If a browser session is driving this task, reload the blocked page so
        // it retries now that the host is allowed (no-op when there's no browser).
        reloadBrowser(key);
      }
      return Response.json({ online: true });
    }

    const sandbox = await getSandbox(key);
    const started = Date.now();
    await sandbox.ping();
    return Response.json({ online: true, latencyMs: Date.now() - started });
  } catch (err) {
    return Response.json({ online: false, error: msg(err) });
  }
}

/**
 * Build the deep link into the local Hiver inspector UI, e.g.
 * `http://localhost:5173/#/sandboxes/<id>/<key>`. Derived from the gateway
 * host with the inspector's default dev port.
 */
function inspectorUrl(id: string, key: string): string {
  const gateway = currentGatewayUrl();
  let host = "localhost";
  try {
    host = new URL(gateway).hostname;
  } catch {
    /* keep default */
  }
  return `http://${host}:5173/#/sandboxes/${id}/${key}`;
}
