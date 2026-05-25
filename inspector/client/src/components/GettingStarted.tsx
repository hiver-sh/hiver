import { useState } from "react";
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


export function GettingStarted() {
  const [lang, setLang] = useState<"ts" | "py">("ts");
  const code = lang === "ts" ? TS_EXAMPLE : PY_EXAMPLE;

  return (
    <div className="h-full overflow-y-auto">
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
      </div>
    </div>
  );
}
