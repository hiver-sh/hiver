import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { CodeTabs } from "@/components/CodeTabs";
import { DEFAULT_GATEWAY_URL } from "@/types";

export interface PortUsageDialogProps {
  sandboxKey: string;
  /** The gateway the inspector is pointed at; surfaced in the snippet when non-default. */
  gatewayUrl: string;
  open: boolean;
  /** The exposed port to document, or null when the sandbox exposes none. */
  port: number | null;
  onOpenChange: (open: boolean) => void;
}

function tsSnippet(key: string, port: number | null, gateway: string | null): string {
  const opts = gateway ? `, {}, { gatewayUrl: ${JSON.stringify(gateway)} }` : "";
  const head = `// npm install --save @hiver.sh/client
import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox(${JSON.stringify(key)}${opts});`;
  if (port === null) return head;
  return `${head}

// proxyUrl(port) returns the base URL that proxies to port ${port}
// inside the sandbox. Append a path to reach a specific endpoint.
const res = await fetch(\`\${sandbox.proxyUrl(${port})}/\`);
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

    # proxy_url(port) returns the base URL that proxies to port ${port}
    # inside the sandbox. Append a path to reach a specific endpoint.
    res = httpx.get(sandbox.proxy_url(${port}) + "/")
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

// ProxyURL(port) proxies to a port inside the sandbox.
res, _ := http.Get(sandbox.ProxyURL(${port}) + "/")
defer res.Body.Close()

body, _ := io.ReadAll(res.Body)
fmt.Println(res.StatusCode, string(body))`;
}

/**
 * Shows how to reach a sandbox's exposed port from the client SDKs.
 * When the sandbox exposes no ports, just shows how to connect to it.
 * Opened from the port chips in the sandbox header.
 */
export function PortUsageDialog({
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
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-2xl">
        <DialogTitle className="font-mono text-sm font-normal text-muted-foreground">
          {port !== null
            ? `Connect to port :${port}`
            : "Connect to the sandbox"}
        </DialogTitle>
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
