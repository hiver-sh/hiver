import { useState } from "react";
import { Check, Clipboard } from "lucide-react";
import { CodeViewer } from "@/components/CodeViewer";

const TS_EXAMPLE = `import * as hive from "hive";

const sandbox = await hive.getOrCreateSandbox("node-sandbox", {
  image: "hive-node-sandbox",
  entrypoint: "tail -f /dev/null",
  fs: [{ mount: "/workspace", backend: "local" }],
});

const exec = await sandbox.execStream(
  \`node -e "console.log('Hello, world!')"\`,
  { cwd: "/workspace" },
);

for await (const pipe of exec.pipes) {
  if (pipe.stdout) process.stdout.write(pipe.stdout);
  if (pipe.stderr) process.stderr.write(pipe.stderr);
}

console.log("exit code:", await exec.exitCode);`;

const PY_EXAMPLE = `import asyncio
import sys
import hive

async def main():
    sandbox = await hive.get_or_create_sandbox(
        "python-sandbox",
        hive.SandboxConfig(
            image="hive-python-sandbox",
            entrypoint="tail -f /dev/null",
            fs=[hive.LocalFileSystem(mount="/workspace", backend="local")],
        ),
    )

    script = "print('Hello, world!')"
    exec = await sandbox.exec_stream(f"python3 -c '{script}'", cwd="/workspace")

    async for pipe in exec.pipes:
        if pipe.stdout:
            print(pipe.stdout, end="")
        if pipe.stderr:
            print(pipe.stderr, end="", file=sys.stderr)

    print("exit code:", await exec.exit_code)

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
