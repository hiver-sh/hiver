import { Loader2, Plus } from "lucide-react";
import { useState } from "react";
import { useTransport } from "@/lib/transport";
import { useSandboxCommand } from "@/lib/useSandboxCommand";
import { CodeEditor } from "@/components/CodeEditor";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { SandboxConfigTemplates } from "@/components/SandboxConfigTemplates";
import type { AnyConfig } from "@/components/SandboxConfigTemplates";

const DEFAULT_CONFIG = {
  fs: [{ backend: "local", mount: "/workspace" }],
  env: {},
};

interface Props {
  serverUrl: string;
  onCreated: (key: string, command: string) => void;
}

export function CreateSandboxDialog({
  serverUrl,
  onCreated,
  compact,
}: Props & { compact?: boolean }) {
  const { transport } = useTransport();
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const [key, setKey] = useState("");
  const [configJson, setConfigJson] = useState(
    JSON.stringify(DEFAULT_CONFIG, null, 2),
  );
  const [command, setCommand, persistCommand] = useSandboxCommand(key);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);

    if (!/^[A-Za-z0-9_-]{1,64}$/.test(key)) {
      setError("Key: only letters, numbers, _ and - (1–64 chars)");
      return;
    }

    let config: Record<string, unknown>;
    try {
      config = JSON.parse(configJson);
    } catch {
      setError("Config is not valid JSON");
      return;
    }

    setLoading(true);
    try {
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(key)}`,
      );
      const res = await transport.fetch(url, {
        method: "PUT",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(config),
      });
      if (!res.ok) {
        const body = await res.json().catch(() => ({ error: res.statusText }));
        setError((body as { error?: string }).error ?? res.statusText);
        return;
      }
      persistCommand();
      setOpen(false);
      onCreated(key, command.trim());
    } catch (err) {
      setError(String(err));
    } finally {
      setLoading(false);
    }
  }

  function handleOpenChange(next: boolean) {
    setOpen(next);
    if (next) {
      setKey("");
      setConfigJson(JSON.stringify(DEFAULT_CONFIG, null, 2));
      setError(null);
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger asChild>
        {compact ? (
          <Button size="icon" variant="ghost" className="h-7 w-7">
            <Plus className="h-4 w-4" />
          </Button>
        ) : (
          <Button size="sm" className="gap-1.5 flex-1">
            <Plus className="h-4 w-4" />
            New Sandbox
          </Button>
        )}
      </DialogTrigger>
      <DialogContent className="sm:max-w-4xl">
        <DialogHeader>
          <DialogTitle>Create Sandbox</DialogTitle>
          <DialogDescription>
            Provision a new sandbox on the controller.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="grid gap-4 py-2">
          <div className="grid gap-1.5">
            <Label htmlFor="sb-key">Key</Label>
            <Input
              id="sb-key"
              placeholder="agent-1"
              value={key}
              onChange={(e) => setKey(e.target.value)}
              disabled={loading}
            />
          </div>
          <div className="grid gap-1.5">
            <div className="flex items-center gap-2">
              <Label>Config</Label>
              <SandboxConfigTemplates
                disabled={loading}
                onApply={(apply) => {
                  try {
                    const current = JSON.parse(configJson) as AnyConfig;
                    setConfigJson(JSON.stringify(apply(current), null, 2));
                  } catch {
                    setError(
                      "Current config is not valid JSON — fix it before applying a template",
                    );
                  }
                }}
                onSuggestId={setKey}
                onSuggestCommand={setCommand}
              />
            </div>
            <CodeEditor
              value={configJson}
              onChange={setConfigJson}
              className="min-h-[320px]"
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="sb-command">Launch Command</Label>
            <Input
              id="sb-command"
              placeholder="/bin/sh"
              value={command}
              onChange={(e) => setCommand(e.target.value)}
              disabled={loading}
            />
          </div>
          {error && <p className="text-sm text-destructive">{error}</p>}
          <DialogFooter>
            <Button type="submit" disabled={loading}>
              {loading && <Loader2 className="animate-spin" />}
              Create
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
