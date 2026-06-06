import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { CodeTabs } from "@/components/CodeTabs";

export interface PortUsageDialogProps {
  sandboxKey: string;
  open: boolean;
  /** The exposed port to document, or null when the sandbox exposes none. */
  port: number | null;
  onOpenChange: (open: boolean) => void;
}

function tsSnippet(key: string, port: number | null): string {
  const head = `import * as hive from "hive-runtime/client";

const sandbox = await hive.getOrCreateSandbox(${JSON.stringify(key)});`;
  if (port === null) return head;
  return `${head}

// proxyUrl(port) returns the base URL that proxies to port ${port}
// inside the sandbox. Append a path to reach a specific endpoint.
const res = await fetch(\`\${sandbox.proxyUrl(${port})}/\`);
console.log(res.status, await res.text());`;
}

function pySnippet(key: string, port: number | null): string {
  const head = `import hive_runtime as hive

sandbox = hive.get_or_create_sandbox(${JSON.stringify(key)})`;
  if (port === null) return head;
  return `${head}

# proxy_url(port) returns the base URL that proxies to port ${port}
# inside the sandbox. Append a path to reach a specific endpoint.
res = hive.get(sandbox.proxy_url(${port}) + "/")
print(res.status_code, res.text)`;
}

function goSnippet(key: string, port: number | null): string {
  const head = `import "github.com/hive-run/hive-runtime/client"

sandbox, _ := hive.GetOrCreateSandbox(${JSON.stringify(key)}, hive.SandboxConfig{})`;
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
export function PortUsageDialog({ sandboxKey, open, port, onOpenChange }: PortUsageDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-2xl">
        <DialogTitle className="font-mono text-sm font-normal text-muted-foreground">
          {port !== null ? `Connect to port :${port}` : "Connect to the sandbox"}
        </DialogTitle>
        <CodeTabs
          examples={{
            ts: tsSnippet(sandboxKey, port),
            py: pySnippet(sandboxKey, port),
            go: goSnippet(sandboxKey, port),
          }}
        />
      </DialogContent>
    </Dialog>
  );
}
