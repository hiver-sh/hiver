import { useState } from "react";
import { Check, Clipboard } from "lucide-react";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { CodeTabs } from "@/components/CodeTabs";
import { DEFAULT_GATEWAY_URL } from "@/types";

export interface PortUsageDialogProps {
  sandboxId: string;
  sandboxKey: string;
  /** The gateway the inspector is pointed at; surfaced in the snippet when non-default. */
  gatewayUrl: string;
  open: boolean;
  /** The exposed port to document, or null when the sandbox exposes none. */
  port: number | null;
  onOpenChange: (open: boolean) => void;
}

/**
 * The full gateway URL that proxies to `port` inside the sandbox, mirroring
 * the client SDK's `sandbox.proxyUrl(port)`
 * (`${gateway}/sandbox/${id}/v1/${key}/proxy/${port}/`, trailing slash included).
 */
function proxyUrl(
  gatewayUrl: string,
  id: string,
  key: string,
  port: number,
): string {
  const base = gatewayUrl.replace(/\/+$/, "");
  return `${base}/sandbox/${encodeURIComponent(id)}/v1/${encodeURIComponent(key)}/proxy/${port}/`;
}

function tsSnippet(key: string, port: number | null, gateway: string | null): string {
  const opts = gateway ? `, {}, { gatewayUrl: ${JSON.stringify(gateway)} }` : "";
  const head = `// npm install --save @hiver.sh/client
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox(${JSON.stringify(key)}${opts});`;
  if (port === null) return head;
  return `${head}

// proxyUrl(port) returns the base URL that proxies to port ${port} inside the
// sandbox (with a trailing slash). Append a path to reach a specific endpoint.
const res = await fetch(sandbox.proxyUrl(${port}));
console.log(res.status, await res.text());`;
}

function pySnippet(key: string, port: number | null, gateway: string | null): string {
  const opts = gateway ? `, gateway_url=${JSON.stringify(gateway)}` : "";
  if (port === null) {
    return `# pip install hiver-py
import asyncio
import hiver

async def main():
    sandbox = await hiver.get_or_create_sandbox(${JSON.stringify(key)}${opts})

asyncio.run(main())`;
  }
  return `# pip install hiver-py
import asyncio
import httpx
import hiver

async def main():
    sandbox = await hiver.get_or_create_sandbox(${JSON.stringify(key)}${opts})

    # proxy_url(port) returns the base URL that proxies to port ${port} inside
    # the sandbox (with a trailing slash). Append a path to reach an endpoint.
    res = httpx.get(sandbox.proxy_url(${port}))
    print(res.status_code, res.text)

asyncio.run(main())`;
}

function goSnippet(key: string, port: number | null, gateway: string): string {
  const head = `// go get github.com/hiver-sh/hiver/client
import "github.com/hiver-sh/hiver/client"

c := client.NewClient(${JSON.stringify(gateway)})
sandbox, _ := c.GetOrCreateSandbox(context.Background(), ${JSON.stringify(key)}, client.SandboxConfig{})`;
  if (port === null) return head;
  return `${head}

// ProxyURL(port) proxies to a port inside the sandbox (with a trailing slash).
res, _ := http.Get(sandbox.ProxyURL(${port}))
defer res.Body.Close()

body, _ := io.ReadAll(res.Body)
fmt.Println(res.StatusCode, string(body))`;
}

const SCHEMES = ["http", "https", "ws", "wss"] as const;
type Scheme = (typeof SCHEMES)[number];

/**
 * The proxy URL rendered as read-only text with a scheme dropdown
 * (http/https/ws/wss) and a copy button (never navigates). The dropdown swaps
 * the scheme on the gateway's http(s) base URL; the initial selection matches
 * the base's TLS-ness.
 */
function ProxyUrlField({ url }: { url: string }) {
  const [scheme, setScheme] = useState<Scheme>(
    /^https:/.test(url) ? "https" : "http",
  );
  const [copied, setCopied] = useState(false);
  // The dropdown carries the scheme, so display/copy just the host+path (which
  // already ends with a slash) with the selected scheme swapped in.
  const rest = url.replace(/^[a-z]+:\/\//, "");
  const shown = `${scheme}://${rest}`;
  const copy = () => {
    navigator.clipboard.writeText(shown);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };
  return (
    <div className="flex w-full min-w-0 items-center gap-2 rounded-md border bg-muted/50 px-3 py-2">
      <select
        value={scheme}
        onChange={(e) => setScheme(e.target.value as Scheme)}
        title="Scheme"
        className="shrink-0 cursor-pointer rounded border bg-background px-1.5 py-0.5 font-mono text-xs text-muted-foreground focus:outline-none"
      >
        {SCHEMES.map((s) => (
          <option key={s} value={s}>
            {s}
          </option>
        ))}
      </select>
      <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground">
        {rest}
      </span>
      <button
        type="button"
        onClick={copy}
        title="Copy to clipboard"
        className="shrink-0 text-muted-foreground hover:text-foreground"
      >
        {copied ? (
          <Check className="h-3.5 w-3.5" />
        ) : (
          <Clipboard className="h-3.5 w-3.5" />
        )}
      </button>
    </div>
  );
}

/** A read-only line of text with a copy button (never navigates). */
function CopyLine({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard.writeText(text);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };
  return (
    <div className="flex w-full min-w-0 items-center gap-2 rounded-md border bg-muted/50 px-3 py-2">
      <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground">
        {text}
      </span>
      <button
        type="button"
        onClick={copy}
        title="Copy to clipboard"
        className="shrink-0 text-muted-foreground hover:text-foreground"
      >
        {copied ? (
          <Check className="h-3.5 w-3.5" />
        ) : (
          <Clipboard className="h-3.5 w-3.5" />
        )}
      </button>
    </div>
  );
}

/**
 * Shows how to reach a sandbox's exposed port from the client SDKs.
 * When the sandbox exposes no ports, just shows how to connect to it.
 * Opened from the port chips in the sandbox header.
 */
export function PortUsageDialog({
  sandboxId,
  sandboxKey,
  gatewayUrl,
  open,
  port,
  onOpenChange,
}: PortUsageDialogProps) {
  // Only thread the gateway through the TS/Python snippets when it differs from
  // the SDK default — otherwise the extra argument is just noise. The Go client
  // always takes the gateway explicitly, so it gets the effective URL.
  const gateway = gatewayUrl === DEFAULT_GATEWAY_URL ? null : gatewayUrl;
  const url =
    port !== null ? proxyUrl(gatewayUrl, sandboxId, sandboxKey, port) : null;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-2xl">
        <DialogTitle className="text-base font-semibold">Connect</DialogTitle>

        <p className="text-sm text-muted-foreground">
          Open an interactive shell with the Hiver CLI:
        </p>
        <CopyLine text={`hiver shell ${sandboxKey}`} />
        
        {url !== null && (
          <>
            <p className="text-sm text-muted-foreground">
              Use the URL below to reach port :{port} on this sandbox directly:
            </p>
            <ProxyUrlField url={url} />
          </>
        )}
        <p className="text-sm text-muted-foreground">
          Connect with the Hiver client:
        </p>
        <CodeTabs
          examples={{
            ts: tsSnippet(sandboxKey, port, gateway),
            py: pySnippet(sandboxKey, port, gateway),
            go: goSnippet(sandboxKey, port, gatewayUrl),
          }}
        />
      </DialogContent>
    </Dialog>
  );
}
