import { useEffect, useState } from "react";
import { Camera, Loader2 } from "lucide-react";
import { useTransport } from "@/lib/transport";
import { Button } from "@/components/ui/button";
import { CodeEditor } from "@/components/CodeEditor";
import { CodeViewer } from "@/components/CodeViewer";
import { LangIcon } from "@/components/LangIcon";
import { SegmentedControl } from "@/components/SegmentedControl";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { cn } from "@/lib/utils";
import { DEFAULT_GATEWAY_URL } from "@/types";

type View = "snapshot" | "restore";
type Lang = "ts" | "py" | "go";

const LANGS: { id: Lang; icon: string; viewer: string }[] = [
  { id: "ts", icon: "typescript", viewer: "typescript" },
  { id: "py", icon: "python", viewer: "python" },
  { id: "go", icon: "go", viewer: "go" },
];

interface Props {
  sandboxId: string;
  sandboxKey: string;
  serverUrl: string;
  /** The gateway the inspector is pointed at; surfaced in the usage snippet when non-default. */
  gatewayUrl: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

interface PartResult {
  captured: boolean;
  key: string;
  bytes?: number;
  reason?: string;
}

interface SnapshotResult {
  vm?: PartResult;
  files?: PartResult;
}

// The `snapshot` block from the sandbox's config (a Snapshot, not a result):
// the restore keys a resume reads from, plus the files include globs.
interface SnapshotConfig {
  vm?: { key: string };
  files?: { key: string; include?: string[] };
}

// The sandbox config may pin restore keys under `snapshot`; when present those
// are the keys a later resume reads from, so the capture defaults to writing
// them. Otherwise we derive a key from the sandbox key so a captured snapshot
// is addressable without the user having to invent one. The files `include`
// globs, when configured, are carried over so the capture covers the same paths.
function defaultConfig(
  sandboxKey: string,
  configSnapshot: SnapshotConfig | undefined,
): string {
  const vmKey = configSnapshot?.vm?.key ?? `${sandboxKey}-vm-snapshot`;
  const filesKey = configSnapshot?.files?.key ?? `${sandboxKey}-files-snapshot`;
  const files: { key: string; include?: string[] } = { key: filesKey };
  if (configSnapshot?.files?.include?.length)
    files.include = configSnapshot.files.include;
  return JSON.stringify(
    {
      vm: { key: vmKey },
      files,
    },
    null,
    2,
  );
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KiB", "MiB", "GiB"];
  let v = bytes / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

// The snapshot is "used" by starting a sandbox configured to restore from the
// keys being captured: get-or-create resumes the VM and rehydrates the files
// instead of cold-booting. The usage example is built from the keys the user
// has in the editor, so it tracks edits live (invalid JSON yields no parts).
function partsFromConfig(config: string): { vm?: string; files?: string } {
  try {
    const parsed = JSON.parse(config) as SnapshotConfig;
    const parts: { vm?: string; files?: string } = {};
    if (parsed.vm?.key) parts.vm = parsed.vm.key;
    if (parsed.files?.key) parts.files = parsed.files.key;
    return parts;
  } catch {
    return {};
  }
}

function tsSnippet(
  key: string,
  parts: { vm?: string; files?: string },
  gateway: string | null,
): string {
  const lines: string[] = [];
  if (parts.vm !== undefined)
    lines.push(`    vm: { key: ${JSON.stringify(parts.vm)} },`);
  if (parts.files !== undefined)
    lines.push(`    files: { key: ${JSON.stringify(parts.files)} },`);
  const opts = gateway ? `, { gatewayUrl: ${JSON.stringify(gateway)} }` : "";
  return `import * as hiver from "@hiver.sh/client";

// Start a sandbox that restores from the snapshot captured above.
const sandbox = await hiver.getOrCreateSandbox(${JSON.stringify(key)}, {
  snapshot: {
${lines.join("\n")}
  },
}${opts});`;
}

function pySnippet(
  key: string,
  parts: { vm?: string; files?: string },
  gateway: string | null,
): string {
  const inner: string[] = [];
  if (parts.vm !== undefined)
    inner.push(
      `            vm=hiver.SnapshotVM(key=${JSON.stringify(parts.vm)}),`,
    );
  if (parts.files !== undefined)
    inner.push(
      `            files=hiver.SnapshotFiles(key=${JSON.stringify(parts.files)}),`,
    );
  const opts = gateway ? `, gateway_url=${JSON.stringify(gateway)}` : "";
  return `import asyncio
import hiver

async def main():
    # Start a sandbox that restores from the snapshot captured above.
    sandbox = await hiver.get_or_create_sandbox(
        ${JSON.stringify(key)},
        hiver.SandboxConfig(
            snapshot=hiver.Snapshot(
${inner.join("\n")}
            ),
        )${opts},
    )

asyncio.run(main())`;
}

function goSnippet(
  key: string,
  parts: { vm?: string; files?: string },
  gateway: string,
): string {
  const inner: string[] = [];
  if (parts.vm !== undefined)
    inner.push(
      `\t\tVM:    &client.SnapshotVM{Key: ${JSON.stringify(parts.vm)}},`,
    );
  if (parts.files !== undefined)
    inner.push(
      `\t\tFiles: &client.SnapshotFiles{Key: ${JSON.stringify(parts.files)}},`,
    );
  return `import "github.com/hiver-sh/hiver/client"

c := client.NewClient(${JSON.stringify(gateway)})

// Start a sandbox that restores from the snapshot captured above.
sandbox, _ := c.GetOrCreateSandbox(context.Background(), ${JSON.stringify(key)}, client.SandboxConfig{
\tSnapshot: &client.Snapshot{
${inner.join("\n")}
\t},
})`;
}

export function SnapshotDialog({
  sandboxId,
  sandboxKey,
  serverUrl,
  gatewayUrl,
  open,
  onOpenChange,
}: Props) {
  const { transport } = useTransport();
  const [config, setConfig] = useState("");
  const [capturing, setCapturing] = useState(false);
  const [result, setResult] = useState<SnapshotResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [view, setView] = useState<View>("snapshot");
  const [lang, setLang] = useState<Lang>("ts");

  // Seed the editor with sensible keys: prefer the sandbox's configured restore
  // keys, falling back to ones derived from the sandbox key.
  useEffect(() => {
    if (!open) return;
    setResult(null);
    setError(null);
    setView("snapshot");
    setConfig(defaultConfig(sandboxKey, undefined));
    const url = new URL(
      `${serverUrl}/api/sandboxes/${encodeURIComponent(sandboxId)}/${encodeURIComponent(sandboxKey)}/config`,
    );
    transport
      .fetch(url)
      .then((r) => r.json())
      .then((data: { snapshot?: SnapshotConfig }) => {
        setConfig(defaultConfig(sandboxKey, data.snapshot));
      })
      .catch(() => {
        /* keep the derived defaults */
      });
  }, [open, sandboxId, sandboxKey, serverUrl, transport]);

  async function handleCapture() {
    let body: string;
    try {
      // Re-stringify so an invalid edit fails here rather than at the sandbox.
      body = JSON.stringify(JSON.parse(config));
    } catch {
      setError("Config is not valid JSON.");
      return;
    }
    setCapturing(true);
    setError(null);
    setResult(null);
    try {
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(sandboxId)}/${encodeURIComponent(sandboxKey)}/snapshot`,
      );
      const res = await transport.fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body,
      });
      const data = await res.json();
      if (!res.ok) {
        setError(data?.error ?? `Request failed (${res.status}).`);
        return;
      }
      setResult(data as SnapshotResult);
    } catch (err) {
      setError(String(err));
    } finally {
      setCapturing(false);
    }
  }

  // Only thread the gateway through the TS/Python snippets when it differs from
  // the SDK default — otherwise the extra argument is just noise. The Go client
  // always takes the gateway explicitly, so it gets the effective URL.
  const gateway = gatewayUrl === DEFAULT_GATEWAY_URL ? null : gatewayUrl;
  const parts = partsFromConfig(config);
  const hasParts = parts.vm !== undefined || parts.files !== undefined;
  const examples: Record<Lang, string> = {
    ts: tsSnippet(sandboxKey, parts, gateway),
    py: pySnippet(sandboxKey, parts, gateway),
    go: goSnippet(sandboxKey, parts, gatewayUrl),
  };
  const activeViewer = LANGS.find((l) => l.id === lang)!.viewer;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="max-w-2xl"
        onKeyDown={(e) => {
          if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && !capturing)
            handleCapture();
        }}
      >
        <DialogTitle>Capture snapshot</DialogTitle>

        <p className="text-sm text-muted-foreground">
          Capture the running sandbox's state without stopping it, so a later
          get-or-create resumes instead of cold-booting. Two independent parts:{" "}
          <span className="font-medium">vm</span> (full microVM state) and{" "}
          <span className="font-medium">files</span> (the writable filesystem).
        </p>

        <div className="flex items-center justify-between">
          <SegmentedControl
            options={[
              { value: "snapshot", label: "Take snapshot" },
              { value: "restore", label: "Restore example" },
            ]}
            value={view}
            onChange={(v) => setView(v as View)}
          />
          {view === "restore" && hasParts && (
            <div className="flex gap-0.5 rounded-md border border-border p-0.5">
              {LANGS.map(({ id, icon }) => (
                <button
                  key={id}
                  onClick={() => setLang(id)}
                  title={id}
                  className={cn(
                    "flex items-center rounded px-2 py-0.5 transition-colors",
                    lang === id
                      ? "bg-muted text-foreground"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  <LangIcon lang={icon} className="h-3.5 w-3.5" />
                </button>
              ))}
            </div>
          )}
        </div>

        {view === "snapshot" ? (
          <div className="flex flex-col gap-1.5">
            <CodeEditor
              value={config}
              onChange={setConfig}
              schema="snapshot"
              autoSize
            />
            <div className="flex justify-end">
              <Button size="sm" disabled={capturing} onClick={handleCapture}>
                {capturing ? (
                  <Loader2 className="h-3 w-3 animate-spin" />
                ) : (
                  <Camera className="h-3 w-3" />
                )}
                Capture
              </Button>
            </div>
          </div>
        ) : (
          <div className="flex flex-col gap-1.5">
            {hasParts ? (
              <div className="overflow-hidden rounded-md border border-border">
                <CodeViewer
                  content={examples[lang]}
                  lang={activeViewer}
                  autoSize
                />
              </div>
            ) : (
              <div className="flex items-center justify-center rounded-md border border-dashed border-border px-4 py-12 text-center text-sm text-muted-foreground">
                Add a vm or files key to see the example.
              </div>
            )}
          </div>
        )}

        {error && (
          <div className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
            {error}
          </div>
        )}

        {result && (
          <div className="space-y-1.5 rounded-md border border-border bg-muted/40 p-3 text-sm">
            {(["vm", "files"] as const).map((part) => {
              const r = result[part];
              if (!r) return null;
              return (
                <div key={part} className="flex items-center gap-2">
                  <span className="w-12 shrink-0 font-mono text-xs text-muted-foreground">
                    {part}
                  </span>
                  {r.captured ? (
                    <span className="text-green-700 dark:text-green-400">
                      captured{" "}
                      <span className="font-mono text-muted-foreground">
                        {r.key}
                      </span>
                      {r.bytes !== undefined && (
                        <span className="text-muted-foreground">
                          {" "}
                          ({formatBytes(r.bytes)})
                        </span>
                      )}
                    </span>
                  ) : (
                    <span className="text-muted-foreground">
                      skipped{r.reason ? ` (${r.reason})` : ""}
                    </span>
                  )}
                </div>
              );
            })}
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
