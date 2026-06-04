import { useState } from "react";
import { Check, Clipboard } from "lucide-react";
import { CodeViewer } from "@/components/CodeViewer";
import { LangIcon } from "@/components/LangIcon";

type Lang = "ts" | "py" | "go";

const LANG_TABS: { id: Lang; icon: string }[] = [
  { id: "ts", icon: "typescript" },
  { id: "py", icon: "python" },
  { id: "go", icon: "go" },
];

const TS_EXAMPLE = `import * as hive from "hive-runtime/client";

const sandbox = await hive.getOrCreateSandbox("agent-1");

const result = await sandbox.exec("claude -p 'Write a poem and save it as pdf'");
console.log(result.stdout);`;

const PY_EXAMPLE = `import hive_runtime as hive

sandbox = hive.get_or_create_sandbox("agent-1")

result = sandbox.exec("claude -p 'Write a poem and save it as pdf'")
print(result.stdout)`;

const GO_EXAMPLE = `import "github.com/hive-run/hive-runtime/client"

sandbox, _ := hive.GetOrCreateSandbox("agent-1", hive.SandboxConfig{})

result, _ := sandbox.Exec("claude -p 'Write a poem and save it as pdf'")
fmt.Println(result.Stdout)`;


export function GettingStarted({ gatewayUrl }: { gatewayUrl: string }) {
  const [lang, setLang] = useState<Lang>("ts");
  const [copied, setCopied] = useState(false);
  const code =
    lang === "ts" ? TS_EXAMPLE : lang === "py" ? PY_EXAMPLE : GO_EXAMPLE;

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

        <div className="flex flex-col gap-2">
          <div className="flex items-center justify-between">
            <div className="flex gap-0.5 rounded-md border border-border p-0.5 text-xs">
              {LANG_TABS.map(({ id, icon }) => (
                <button
                  key={id}
                  onClick={() => setLang(id)}
                  className={`flex items-center gap-1.5 rounded px-2.5 py-1 transition-colors ${
                    lang === id
                      ? "bg-muted text-foreground"
                      : "text-muted-foreground hover:text-foreground"
                  }`}
                >
                  <LangIcon lang={icon} className="h-3.5 w-3.5" />
                </button>
              ))}
            </div>
          </div>

          <div className="rounded-lg overflow-hidden border border-border">
            <CodeViewer
              content={code}
              lang={lang === "ts" ? "typescript" : lang === "py" ? "python" : "go"}
              autoSize
            />
          </div>
        </div>

        <div className="flex flex-col gap-2">
          <h2 className="text-sm font-medium">Gateway URL</h2>
          <p className="text-sm text-muted-foreground">
            Set this environment variable so the SDK can reach the controller.
          </p>
          <div className="flex items-center gap-2 rounded-lg border border-border bg-muted/40 px-3 py-2">
            <code className="flex-1 font-mono text-xs text-foreground select-all">
              HIVE_GATEWAY_URL={gatewayUrl}
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
