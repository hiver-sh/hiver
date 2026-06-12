import { useState } from "react";
import { CodeTabs } from "@/components/CodeTabs";

const TS_EXAMPLE = `import * as hiver from "@hiver.sh/client";

const sandbox = await hiver.getOrCreateSandbox("agent-1");

const result = await sandbox.exec("claude -p 'Write a poem and save it as pdf'");
console.log(result.stdout);`;

const PY_EXAMPLE = `import hiver

sandbox = hiver.get_or_create_sandbox("agent-1")

result = sandbox.exec("claude -p 'Write a poem and save it as pdf'")
print(result.stdout)`;

const GO_EXAMPLE = `import "github.com/hiver-sh/runtime/client"

sandbox, _ := hive.GetOrCreateSandbox("agent-1", hive.SandboxConfig{})

result, _ := sandbox.Exec("claude -p 'Write a poem and save it as pdf'")
fmt.Println(result.Stdout)`;

export function GettingStarted({ gatewayUrl }: { gatewayUrl: string }) {
  const [copied, setCopied] = useState(false);

  function handleCopy() {
    navigator.clipboard.writeText(gatewayUrl);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }

  return (
    <div className="h-full overflow-y-auto scroll-container">
      <div className="w-full max-w-4xl mx-auto flex flex-col gap-6 px-8 py-16">
        <div className="flex flex-col gap-1.5">
          <h1 className="text-lg font-semibold">Get started</h1>
          <p className="text-sm text-muted-foreground">
            Create a sandbox to run and inspect your agent. Hive monitors egress
            traffic, file system access, and LLM calls in real time.
          </p>
        </div>

        <CodeTabs
          examples={{ ts: TS_EXAMPLE, py: PY_EXAMPLE, go: GO_EXAMPLE }}
        />
      </div>
    </div>
  );
}
