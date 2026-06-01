import { useState } from "react";
import { Check, Clipboard } from "lucide-react";
import { CodeViewer } from "@/components/CodeViewer";

const TS_EXAMPLE = `import * as hive from "hive";
import Anthropic from "@anthropic-ai/sdk";

const sandbox = await hive.getOrCreateSandbox("my-sandbox", {
  fs: [{ mount: "/workspace", backend: "local" }],
  egress: [
    { host: "api.anthropic.com", access: "allow" },
    { host: "*.amazonaws.com", ports: [443], access: "allow" },
  ],
});

const client = new Anthropic({ baseURL: \`\${sandbox.apiServerUrl}/proxy\` });

const message = await client.messages.create({
  model: "claude-opus-4-7",
  max_tokens: 1024,
  messages: [{ role: "user", content: "Write a file to /workspace/hello.txt" }],
});
console.log(message.content);`;

const PY_EXAMPLE = `import asyncio
import hive
import anthropic

async def main():
    sandbox = await hive.get_or_create_sandbox(
        "my-sandbox",
        hive.SandboxConfig(
            fs=[hive.LocalFileSystem(mount="/workspace", backend="local")],
            egress=[
                hive.EgressRule(host="api.anthropic.com", access="allow"),
                hive.EgressRule(host="*.amazonaws.com", ports=[443], access="allow"),
            ],
        ),
    )

    client = anthropic.Anthropic(base_url=f"{sandbox.api_server_url}/proxy")

    message = client.messages.create(
        model="claude-opus-4-7",
        max_tokens=1024,
        messages=[{"role": "user", "content": "Write a file to /workspace/hello.txt"}],
    )
    print(message.content)

asyncio.run(main())`;


export function GettingStarted({ controllerUrl }: { controllerUrl: string }) {
  const [lang, setLang] = useState<"ts" | "py">("ts");
  const [copied, setCopied] = useState(false);
  const code = lang === "ts" ? TS_EXAMPLE : PY_EXAMPLE;

  function handleCopy() {
    navigator.clipboard.writeText(controllerUrl);
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

        <div className="flex flex-col gap-2">
          <div className="flex items-center justify-between">
            <div className="flex gap-0.5 rounded-md border border-border p-0.5 text-xs">
              <button
                onClick={() => setLang("ts")}
                className={`rounded px-2.5 py-1 transition-colors ${
                  lang === "ts"
                    ? "bg-muted text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                TypeScript
              </button>
              <button
                onClick={() => setLang("py")}
                className={`rounded px-2.5 py-1 transition-colors ${
                  lang === "py"
                    ? "bg-muted text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                Python
              </button>
            </div>
          </div>

          <div className="rounded-lg overflow-hidden border border-border">
            <CodeViewer
              content={code}
              lang={lang === "ts" ? "typescript" : "python"}
              autoSize
            />
          </div>
        </div>

        <div className="flex flex-col gap-2">
          <h2 className="text-sm font-medium">Controller URL</h2>
          <p className="text-sm text-muted-foreground">
            Set this environment variable so the SDK can reach the controller.
          </p>
          <div className="flex items-center gap-2 rounded-lg border border-border bg-muted/40 px-3 py-2">
            <code className="flex-1 font-mono text-xs text-foreground select-all">
              HIVE_CONTROLLER_URL={controllerUrl}
            </code>
            <button
              onClick={handleCopy}
              className="shrink-0 p-1 rounded text-muted-foreground hover:text-foreground transition-colors"
              title="Copy"
            >
              {copied ? <Check className="h-3.5 w-3.5" /> : <Clipboard className="h-3.5 w-3.5" />}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
