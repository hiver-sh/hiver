"use client";

import * as React from "react";
import { ExternalLink, Loader2, RefreshCw, X, Zap } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import {
  ORCHESTRATION_ORDER,
  ORCHESTRATIONS,
  type Orchestration,
} from "@/lib/orchestration";

type SandboxStatus = {
  online: boolean;
  key: string;
  image: string;
  id?: string;
  isolation?: string;
  ports?: number[];
  inspectorUrl?: string;
  error?: string;
};

type PingResult = { online: boolean; latencyMs?: number; error?: string };

export function SettingsPanel({
  open,
  onClose,
  sandboxKey,
  ttlMinutes,
  onTtlChange,
  gatewayUrl,
  onGatewayUrlChange,
  apiKeys,
  onApiKeyChange,
}: {
  open: boolean;
  onClose: () => void;
  /** The active task's id = its sandbox key; null when no task is open. */
  sandboxKey: string | null;
  ttlMinutes: number;
  onTtlChange: (minutes: number) => void;
  gatewayUrl: string;
  onGatewayUrlChange: (url: string) => void;
  apiKeys: Record<Orchestration, string>;
  onApiKeyChange: (orchestration: Orchestration, key: string) => void;
}) {
  const [status, setStatus] = React.useState<SandboxStatus | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [pinging, setPinging] = React.useState(false);
  const [ping, setPing] = React.useState<PingResult | null>(null);

  const refresh = React.useCallback(async () => {
    if (!sandboxKey) {
      setStatus({ online: false, key: "", image: "", error: "No task open." });
      return;
    }
    setLoading(true);
    setPing(null);
    try {
      const res = await fetch(
        `/api/sandbox?key=${encodeURIComponent(sandboxKey)}&gatewayUrl=${encodeURIComponent(gatewayUrl)}`,
        { cache: "no-store" },
      );
      setStatus(await res.json());
    } catch (err) {
      setStatus({
        online: false,
        key: "",
        image: "",
        error: err instanceof Error ? err.message : String(err),
      });
    } finally {
      setLoading(false);
    }
  }, [sandboxKey, gatewayUrl]);

  React.useEffect(() => {
    if (open) refresh();
  }, [open, refresh]);

  // Close on Escape.
  React.useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (!open) return null;

  async function doPing() {
    if (!sandboxKey) return;
    setPinging(true);
    setPing(null);
    try {
      const res = await fetch("/api/sandbox", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ action: "ping", key: sandboxKey, gatewayUrl }),
      });
      setPing(await res.json());
    } catch (err) {
      setPing({ online: false, error: err instanceof Error ? err.message : String(err) });
    } finally {
      setPinging(false);
    }
  }

  // Apply the TTL (minutes → seconds) to the running sandbox via applyConfig.
  async function applyTtl() {
    if (!sandboxKey) return;
    try {
      await fetch("/api/sandbox", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          action: "ttl",
          key: sandboxKey,
          ttl: Math.max(0, Math.round(ttlMinutes)) * 60,
          gatewayUrl,
        }),
      });
      await refresh();
    } catch {
      /* status refresh will surface any error */
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
    >
      <div
        className="absolute inset-0 bg-black/50 backdrop-blur-sm"
        onClick={onClose}
      />
      <div className="relative z-10 w-full max-w-md rounded-xl border bg-card shadow-xl">
        <div className="flex items-center justify-between border-b px-4 py-3">
          <span className="text-sm font-semibold">Settings</span>
          <Button variant="ghost" size="icon-sm" onClick={onClose} aria-label="Close">
            <X />
          </Button>
        </div>

        <div className="space-y-5 p-4">
          {/* Sandbox status */}
          <section>
            <div className="mb-2 flex items-center justify-between">
              <h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
                Sandbox
              </h3>
              <Button
                variant="ghost"
                size="icon-sm"
                onClick={refresh}
                disabled={loading}
                aria-label="Refresh status"
              >
                <RefreshCw className={cn(loading && "animate-spin")} />
              </Button>
            </div>

            <div className="rounded-lg border bg-background p-3 text-xs">
              <div className="flex items-center gap-2">
                <span
                  className={cn(
                    "size-2 rounded-full",
                    loading
                      ? "bg-amber-500 animate-pulse"
                      : status?.online
                        ? "bg-emerald-500"
                        : "bg-destructive",
                  )}
                />
                <span className="font-medium">
                  {loading ? "Checking…" : status?.online ? "Online" : "Offline"}
                </span>
              </div>

              <dl className="mt-2 space-y-1 text-muted-foreground">
                <Row label="Key" value={status?.key} />
                <Row label="Image" value={status?.image} />
                {status?.isolation && <Row label="Isolation" value={status.isolation} />}
                {status?.ports && (
                  <Row label="Ports" value={status.ports.join(", ") || "none"} />
                )}
                {status?.error && (
                  <Row label="Error" value={status.error} valueClass="text-destructive" />
                )}
              </dl>
            </div>

            <div className="mt-2 flex gap-2">
              <Button
                variant="outline"
                size="sm"
                className="flex-1"
                onClick={doPing}
                disabled={pinging}
              >
                {pinging ? <Loader2 className="animate-spin" /> : <Zap />}
                Ping
                {ping?.online && ping.latencyMs !== undefined && (
                  <span className="text-muted-foreground">· {ping.latencyMs}ms</span>
                )}
                {ping && !ping.online && (
                  <span className="text-destructive">· failed</span>
                )}
              </Button>
              <Button
                variant="outline"
                size="sm"
                className="flex-1"
                disabled={!status?.inspectorUrl}
                onClick={() =>
                  status?.inspectorUrl &&
                  window.open(status.inspectorUrl, "_blank", "noopener")
                }
              >
                <ExternalLink />
                Open inspector
              </Button>
            </div>
          </section>

          {/* Hiver gateway */}
          <section className="space-y-1.5">
            <label
              htmlFor="gateway"
              className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
            >
              Hiver gateway URL
            </label>
            <Input
              id="gateway"
              type="text"
              spellCheck={false}
              placeholder="http://localhost:10000"
              value={gatewayUrl}
              onChange={(e) => onGatewayUrlChange(e.target.value)}
              onBlur={refresh}
            />
          </section>

          {/* Time to live */}
          <section className="space-y-1.5">
            <label
              htmlFor="ttl"
              className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
            >
              Time to live (minutes)
            </label>
            <Input
              id="ttl"
              type="number"
              min={0}
              value={ttlMinutes}
              onChange={(e) => onTtlChange(Number(e.target.value))}
              onBlur={applyTtl}
              onKeyDown={(e) => {
                if (e.key === "Enter") (e.target as HTMLInputElement).blur();
              }}
            />
            <p className="text-[11px] text-muted-foreground">
              The sandbox stops after this long without activity. 0 disables it.
            </p>
          </section>

          {/* Provider API keys — the model picker selects which one is used. */}
          {ORCHESTRATION_ORDER.map((o) => {
            const engine = ORCHESTRATIONS[o];
            return (
              <section key={o} className="space-y-1.5">
                <label
                  htmlFor={`api-key-${o}`}
                  className="text-xs font-medium uppercase tracking-wide text-muted-foreground"
                >
                  {engine.keyLabel}
                </label>
                <Input
                  id={`api-key-${o}`}
                  type="password"
                  autoComplete="off"
                  placeholder={`${engine.env}…`}
                  value={apiKeys[o]}
                  onChange={(e) => onApiKeyChange(o, e.target.value)}
                />
              </section>
            );
          })}
        </div>
      </div>
    </div>
  );
}

function Row({
  label,
  value,
  valueClass,
}: {
  label: string;
  value?: string;
  valueClass?: string;
}) {
  return (
    <div className="flex gap-2">
      <dt className="w-16 shrink-0">{label}</dt>
      <dd className={cn("min-w-0 flex-1 break-words font-mono text-foreground", valueClass)}>
        {value ?? "—"}
      </dd>
    </div>
  );
}
