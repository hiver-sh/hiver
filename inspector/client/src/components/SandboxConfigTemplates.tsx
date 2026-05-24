import { ChevronDown } from "lucide-react";
import { useState } from "react";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";

export type AnyConfig = Record<string, unknown>;
type FsEntry = Record<string, unknown>;

type Template = { label: string; apply: (cfg: AnyConfig) => AnyConfig };

export const TEMPLATE_GROUPS: { group: string; templates: Template[] }[] = [
  {
    group: "Images",
    templates: [
      {
        label: "Claude Code",
        apply: () => ({
          image: "hive-example-claude-worker-bundle",
          fs: [{ backend: "local", mount: "/workspace" }],
          env: { CLAUDE_CODE_OAUTH_TOKEN: "<your-key>" },
          egress: [
            { host: "api.anthropic.com", access: "allow", ports: [443] },
            { host: "*.anthropic.com", access: "allow", ports: [443] },
            { host: "*", access: "deny" },
          ],
        }),
      },
      {
        label: "Hive MCP server",
        apply: () => ({
          image: "hive-mcp-server-bundle",
          fs: [{ backend: "local", mount: "/workspace", acls: [{ path: "/workspace/**", access: "rw" }] }],
          env: {},
          egress: [
            { host: "api.anthropic.com", access: "allow", ports: [443] },
            { host: "*", access: "deny" },
          ],
        }),
      },
    ],
  },
  {
    group: "File systems",
    templates: [
      {
        label: "Local files",
        apply: (cfg) => ({
          ...cfg,
          fs: [...((cfg.fs as FsEntry[]) ?? []), { backend: "local", mount: "/data" }],
        }),
      },
      {
        label: "Google Drive",
        apply: (cfg) => ({
          ...cfg,
          fs: [
            ...((cfg.fs as FsEntry[]) ?? []),
            {
              backend: "gdrive",
              mount: "/gdrive",
              gdrive_access_token: "",
              gdrive_refresh_token: "",
              gdrive_client_id: "",
              gdrive_client_secret: "",
            },
          ],
        }),
      },
      {
        label: "OneDrive",
        apply: (cfg) => ({
          ...cfg,
          fs: [...((cfg.fs as FsEntry[]) ?? []), {}],
        }),
      },
      {
        label: "Google Cloud Storage",
        apply: (cfg) => ({
          ...cfg,
          fs: [
            ...((cfg.fs as FsEntry[]) ?? []),
            {
              backend: "gcs",
              mount: "/gcs",
              gcs_bucket: "my-bucket",
              gcs_prefix: "",
              gcs_service_account_json: "",
            },
          ],
        }),
      },
      {
        label: "Amazon S3",
        apply: (cfg) => ({
          ...cfg,
          fs: [...((cfg.fs as FsEntry[]) ?? []), {}],
        }),
      },
      {
        label: "Azure Blob",
        apply: (cfg) => ({
          ...cfg,
          fs: [...((cfg.fs as FsEntry[]) ?? []), {}],
        }),
      },
      {
        label: "Allow file",
        apply: (cfg) => {
          const fs = [...((cfg.fs as FsEntry[]) ?? [])];
          if (fs.length > 0) {
            const first = { ...fs[0] };
            first.acls = [
              ...((first.acls as AnyConfig[]) ?? []),
              { path: `${first.mount ?? "/workspace"}/**`, access: "rw" },
            ];
            fs[0] = first;
          }
          return { ...cfg, fs };
        },
      },
      {
        label: "Deny file",
        apply: (cfg) => {
          const fs = [...((cfg.fs as FsEntry[]) ?? [])];
          if (fs.length > 0) {
            const first = { ...fs[0] };
            first.acls = [
              ...((first.acls as AnyConfig[]) ?? []),
              { path: `${first.mount ?? "/workspace"}/secret/**`, access: "deny" },
            ];
            fs[0] = first;
          }
          return { ...cfg, fs };
        },
      },
    ],
  },
  {
    group: "Networking",
    templates: [
      {
        label: "Allow host",
        apply: (cfg) => ({
          ...cfg,
          egress: [
            ...((cfg.egress as AnyConfig[]) ?? []),
            { host: "example.com", access: "allow", ports: [443] },
          ],
        }),
      },
      {
        label: "Deny host",
        apply: (cfg) => ({
          ...cfg,
          egress: [
            ...((cfg.egress as AnyConfig[]) ?? []),
            { host: "example.com", access: "deny" },
          ],
        }),
      },
    ],
  },
];

interface Props {
  disabled?: boolean;
  editMode?: boolean;
  onApply: (template: (cfg: AnyConfig) => AnyConfig) => void;
}

export function SandboxConfigTemplates({ disabled, editMode, onApply }: Props) {
  const [open, setOpen] = useState(false);
  const groups = TEMPLATE_GROUPS;
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          disabled={disabled}
          className="flex items-center gap-1 rounded-md border border-border px-2 py-0.5 text-xs text-muted-foreground transition-colors hover:bg-muted/40 disabled:opacity-50"
        >
          Add template
          <ChevronDown className="h-3 w-3" />
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-48 p-1">
        {groups.map((group, gi) => {
          const groupDisabled = editMode && group.group === "Images";
          return (
            <div key={gi}>
              {gi > 0 && <div className="my-1 border-t border-border" />}
              <p className={`px-2 py-1 text-[10px] font-medium uppercase tracking-wider ${groupDisabled ? "text-muted-foreground/30" : "text-muted-foreground/60"}`}>
                {group.group}
              </p>
              {group.templates.map((t, ti) => (
                <button
                  key={ti}
                  type="button"
                  disabled={groupDisabled}
                  className="w-full rounded-sm px-2 py-1.5 text-left text-xs hover:bg-muted/60 disabled:pointer-events-none disabled:opacity-30"
                  onClick={() => { onApply(t.apply); setOpen(false); }}
                >
                  {t.label}
                </button>
              ))}
            </div>
          );
        })}
      </PopoverContent>
    </Popover>
  );
}
