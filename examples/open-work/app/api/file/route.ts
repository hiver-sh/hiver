import { getSandbox, setGatewayUrl } from "@/lib/hiver";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Serve a single file from a task's sandbox (for the Workspace file viewer).
// GET /api/file?session=<taskId>&path=/workspace/output/report.md
export async function GET(req: Request): Promise<Response> {
  const url = new URL(req.url);
  setGatewayUrl(url.searchParams.get("gatewayUrl") ?? undefined);
  const session = url.searchParams.get("session");
  const path = url.searchParams.get("path");
  if (!session || !path || !path.startsWith("/workspace/")) {
    return new Response("Bad request", { status: 400 });
  }

  try {
    const sandbox = await getSandbox(session);
    const bytes = await sandbox.readFile(path);
    // Uint8Array is a valid runtime BodyInit; the DOM lib types are overly strict.
    return new Response(bytes as unknown as BodyInit, {
      headers: {
        "content-type": contentType(path),
        "cache-control": "no-store",
      },
    });
  } catch {
    return new Response("Not found", { status: 404 });
  }
}

function contentType(path: string): string {
  const ext = path.split(".").pop()?.toLowerCase() ?? "";
  const map: Record<string, string> = {
    md: "text/markdown; charset=utf-8",
    markdown: "text/markdown; charset=utf-8",
    txt: "text/plain; charset=utf-8",
    csv: "text/csv; charset=utf-8",
    json: "application/json; charset=utf-8",
    png: "image/png",
    jpg: "image/jpeg",
    jpeg: "image/jpeg",
    gif: "image/gif",
    webp: "image/webp",
    svg: "image/svg+xml",
    avif: "image/avif",
    bmp: "image/bmp",
  };
  return map[ext] ?? "text/plain; charset=utf-8";
}
