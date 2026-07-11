"use client";

import * as React from "react";
import { ShieldAlert } from "lucide-react";

import { Button } from "@/components/ui/button";

/**
 * A single elicitation for every host the agent was blocked from. Each host has
 * a checkbox (all on by default); "Allow" grants the checked hosts and dismisses
 * the rest, "Dismiss" clears them all.
 */
export function EgressCard({
  hosts,
  onAllow,
  onDismiss,
}: {
  hosts: string[];
  onAllow: (hosts: string[]) => void;
  onDismiss: (hosts: string[]) => void;
}) {
  const [selected, setSelected] = React.useState<Set<string>>(() => new Set(hosts));
  const [busy, setBusy] = React.useState(false);

  // Keep new hosts selected as they stream in, without clobbering user toggles.
  const known = React.useRef<Set<string>>(new Set(hosts));
  React.useEffect(() => {
    setSelected((prev) => {
      const next = new Set(prev);
      for (const h of hosts) if (!known.current.has(h)) next.add(h);
      known.current = new Set(hosts);
      return next;
    });
  }, [hosts]);

  const toggle = (host: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(host)) next.delete(host);
      else next.add(host);
      return next;
    });

  const allow = () => {
    setBusy(true);
    const granted = hosts.filter((h) => selected.has(h));
    const rejected = hosts.filter((h) => !selected.has(h));
    if (granted.length) onAllow(granted);
    if (rejected.length) onDismiss(rejected);
  };

  return (
    <div className="rounded-xl border bg-muted/40 px-4 py-3.5">
      <div className="flex items-center gap-2.5">
        <ShieldAlert className="size-4 shrink-0 text-muted-foreground" />
        <p className="text-sm font-medium">Access blocked</p>
      </div>

      <ul className="mt-2.5 space-y-1.5">
        {hosts.map((host) => (
          <li key={host}>
            <label className="flex cursor-pointer items-center gap-2.5">
              <input
                type="checkbox"
                checked={selected.has(host)}
                disabled={busy}
                onChange={() => toggle(host)}
                className="size-3.5 shrink-0 accent-primary"
              />
              <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground">
                {host}
              </span>
            </label>
          </li>
        ))}
      </ul>

      <div className="mt-3 flex items-center justify-end gap-2">
        <Button variant="ghost" size="sm" disabled={busy} onClick={() => onDismiss(hosts)}>
          Dismiss
        </Button>
        <Button size="sm" disabled={busy || selected.size === 0} onClick={allow}>
          Allow{selected.size > 1 ? ` (${selected.size})` : ""}
        </Button>
      </div>
    </div>
  );
}
