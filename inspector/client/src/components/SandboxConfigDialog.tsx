import { useEffect, useMemo, useState } from "react";
import { useTransport } from "@/lib/transport";
import { Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { CodeEditor } from "@/components/CodeEditor";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { SegmentedControl } from "@/components/SegmentedControl";
import { SandboxConfigTemplates } from "@/components/SandboxConfigTemplates";
import type { AnyConfig } from "@/components/SandboxConfigTemplates";


type DL = { type: "+" | "-" | " "; text: string; oldLine: number; newLine: number };

function computeLineDiff(a: string, b: string): DL[] {
  const aL = a.split("\n"), bL = b.split("\n");
  const m = aL.length, n = bL.length;
  const dp = Array.from({ length: m + 1 }, () => new Array(n + 1).fill(0) as number[]);
  for (let i = 1; i <= m; i++)
    for (let j = 1; j <= n; j++)
      dp[i][j] = aL[i - 1] === bL[j - 1] ? dp[i - 1][j - 1] + 1 : Math.max(dp[i - 1][j], dp[i][j - 1]);

  const lines: DL[] = [];
  function bt(i: number, j: number): void {
    if (!i && !j) return;
    if (i && j && aL[i - 1] === bL[j - 1]) { bt(i - 1, j - 1); lines.push({ type: " ", text: aL[i - 1], oldLine: 0, newLine: 0 }); }
    else if (j && (!i || dp[i][j - 1] >= dp[i - 1][j])) { bt(i, j - 1); lines.push({ type: "+", text: bL[j - 1], oldLine: 0, newLine: 0 }); }
    else { bt(i - 1, j); lines.push({ type: "-", text: aL[i - 1], oldLine: 0, newLine: 0 }); }
  }
  bt(m, n);

  let oldL = 1, newL = 1;
  for (const l of lines) {
    if (l.type === "-") { l.oldLine = oldL++; }
    else if (l.type === "+") { l.newLine = newL++; }
    else { l.oldLine = oldL++; l.newLine = newL++; }
  }
  return lines;
}

interface Hunk { oldStart: number; oldCount: number; newStart: number; newCount: number; lines: DL[] }

function buildHunks(diff: DL[], ctx = 3): Hunk[] {
  const changed = diff.reduce<number[]>((acc, l, i) => (l.type !== " " ? [...acc, i] : acc), []);
  if (!changed.length) return [];
  const ranges: [number, number][] = [];
  let s = Math.max(0, changed[0] - ctx), e = Math.min(diff.length - 1, changed[0] + ctx);
  for (let i = 1; i < changed.length; i++) {
    const lo = Math.max(0, changed[i] - ctx), hi = Math.min(diff.length - 1, changed[i] + ctx);
    if (lo <= e + 1) { e = hi; } else { ranges.push([s, e]); s = lo; e = hi; }
  }
  ranges.push([s, e]);
  return ranges.map(([s, e]) => {
    const ls = diff.slice(s, e + 1);
    const oldLines = ls.filter(l => l.type !== "+");
    const newLines = ls.filter(l => l.type !== "-");
    return {
      oldStart: oldLines[0]?.oldLine ?? 1, oldCount: oldLines.length,
      newStart: newLines[0]?.newLine ?? 1, newCount: newLines.length,
      lines: ls,
    };
  });
}

type JT = { k: "key" | "str" | "num" | "kw" | "ot"; v: string };

function tokenize(line: string): JT[] {
  const out: JT[] = [];
  let i = 0;
  while (i < line.length) {
    if (line[i] === '"') {
      let j = i + 1;
      while (j < line.length) {
        if (line[j] === "\\") j += 2; else if (line[j] === '"') { j++; break; } else j++;
      }
      const isKey = /^\s*:/.test(line.slice(j));
      out.push({ k: isKey ? "key" : "str", v: line.slice(i, j) });
      i = j;
    } else {
      const nm = line.slice(i).match(/^-?\d+(\.\d+)?([eE][+-]?\d+)?/);
      if (nm) { out.push({ k: "num", v: nm[0] }); i += nm[0].length; continue; }
      const kw = line.slice(i).match(/^(true|false|null)/);
      if (kw) { out.push({ k: "kw", v: kw[0] }); i += kw[0].length; continue; }
      const last = out[out.length - 1];
      if (last?.k === "ot") last.v += line[i]; else out.push({ k: "ot", v: line[i] });
      i++;
    }
  }
  return out;
}

function JsonLine({ text }: { text: string }) {
  const tokens = useMemo(() => tokenize(text), [text]);
  return (
    <>
      {tokens.map((t, i) => {
        if (t.k === "key") return <span key={i} className="text-sky-600 dark:text-sky-300">{t.v}</span>;
        if (t.k === "str") return <span key={i} className="text-amber-700 dark:text-amber-300">{t.v}</span>;
        if (t.k === "num") return <span key={i} className="text-violet-600 dark:text-violet-300">{t.v}</span>;
        if (t.k === "kw")  return <span key={i} className="text-blue-600 dark:text-blue-300">{t.v}</span>;
        return <span key={i}>{t.v}</span>;
      })}
    </>
  );
}

// ── Diff view ─────────────────────────────────────────────────────────────────

function DiffView({ oldStr, newStr }: { oldStr: string; newStr: string }) {
  const hunks = useMemo(() => buildHunks(computeLineDiff(oldStr, newStr)), [oldStr, newStr]);
  if (!hunks.length) {
    return (
      <div className="flex min-h-32 h-full items-center justify-center text-sm text-muted-foreground">
        No changes
      </div>
    );
  }
  return (
    <div className="font-mono text-xs select-text">
      {hunks.map((hunk, hi) => (
        <div key={hi}>
          <div className="bg-blue-100/80 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400/80 px-4 py-0.5 select-none text-[11px]">
            @@ -{hunk.oldStart},{hunk.oldCount} +{hunk.newStart},{hunk.newCount} @@
          </div>
          {hunk.lines.map((line, li) => (
            <div
              key={li}
              className={`flex px-4 py-px whitespace-pre ${
                line.type === "+" ? "bg-green-100 text-green-900 dark:bg-green-500/15 dark:text-green-100" :
                line.type === "-" ? "bg-red-100 text-red-900 dark:bg-red-500/15 dark:text-red-100" :
                "text-muted-foreground"
              }`}
            >
              <span className="select-none w-4 shrink-0 opacity-50">{line.type}</span>
              <JsonLine text={line.text} />
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

// ── Dialog ────────────────────────────────────────────────────────────────────

export interface ConfigProposal { current: string; proposed: string }

type Mode = "diff" | "editor";

interface Props {
  sandboxKey: string;
  serverUrl: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  proposal?: ConfigProposal;
}

export function SandboxConfigDialog({ sandboxKey, serverUrl, open, onOpenChange, proposal }: Props) {
  const { transport } = useTransport();
  const [savedConfig, setSavedConfig] = useState("");
  const [editedConfig, setEditedConfig] = useState("");
  const [saving, setSaving] = useState(false);
  const [mode, setMode] = useState<Mode>(() => proposal ? "diff" : "editor");

  const baseConfig = proposal?.current ?? savedConfig;
  const hasChanges = editedConfig !== baseConfig;

  useEffect(() => {
    if (!open) return;
    const url = new URL(`${serverUrl}/api/sandboxes/${encodeURIComponent(sandboxKey)}/config`);
    transport.fetch(url).then((r) => r.json()).then((data) => {
      const str = JSON.stringify(data, null, 2);
      setSavedConfig(str);
      setEditedConfig(proposal?.proposed ?? str);
    });
  }, [open, sandboxKey, serverUrl, proposal, transport]);

  useEffect(() => {
    if (open) setMode(proposal ? "diff" : "editor");
  }, [open, proposal]);

  async function handleSave() {
    setSaving(true);
    try {
      const url = new URL(`${serverUrl}/api/sandboxes/${encodeURIComponent(sandboxKey)}/config`);
        await transport.fetch(url, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: editedConfig,
      });
      setSavedConfig(editedConfig);
      onOpenChange(false);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-4xl" onKeyDown={(e) => { if (e.key === "Enter" && (mode === "diff" || e.metaKey || e.ctrlKey) && !saving && hasChanges) handleSave(); }}>
        <div className="flex items-center justify-between pr-6">
          <DialogTitle>Sandbox config</DialogTitle>
          <div className="flex items-center gap-2">
            {mode === "editor" && (
              <SandboxConfigTemplates
                editMode
                onApply={(apply) => {
                  try {
                    const current = JSON.parse(editedConfig) as AnyConfig;
                    setEditedConfig(JSON.stringify(apply(current), null, 2));
                  } catch { /* ignore invalid JSON */ }
                }}
              />
            )}
            <SegmentedControl
              options={[
                { value: "diff", label: "Diff" },
                { value: "editor", label: "Editor" },
              ]}
              value={mode}
              onChange={(v) => setMode(v as Mode)}
            />
          </div>
        </div>

        {mode === "diff" ? (
          <div className="h-[55vh] overflow-auto scroll-container rounded-md border border-border">
            <DiffView oldStr={baseConfig} newStr={editedConfig} />
          </div>
        ) : (
          <CodeEditor value={editedConfig} onChange={setEditedConfig} className="h-[55vh]" />
        )}

        <div className="flex justify-end">
          <Button size="sm" disabled={saving || !hasChanges} onClick={handleSave}>
            {saving && <Loader2 className="h-3 w-3 animate-spin" />}
            {proposal ? "Apply" : "Save"}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
