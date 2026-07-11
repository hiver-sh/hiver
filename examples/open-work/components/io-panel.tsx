"use client";

import { FileText, PanelRightClose } from "lucide-react";

import { Button } from "@/components/ui/button";
import { ScrollArea } from "@/components/ui/scroll-area";
import { formatBytes } from "@/lib/utils";
import type { Task } from "@/lib/task";
import type { OutputFile } from "@/lib/types";

export function IoPanel({
  task,
  onOpenFile,
  onClose,
}: {
  task: Task | null;
  onOpenFile: (file: OutputFile) => void;
  onClose: () => void;
}) {
  const inputs = task?.inputs ?? [];
  const outputs = task?.outputs ?? [];

  return (
    <aside className="fixed inset-y-0 right-0 z-40 flex h-full w-80 max-w-[85vw] shrink-0 flex-col border-l bg-card shadow-xl lg:static lg:z-auto lg:max-w-none lg:bg-card/40 lg:shadow-none">
      <div className="flex items-center justify-between px-4 py-3">
        <span className="text-sm font-semibold">Workspace</span>
        <Button variant="ghost" size="icon-sm" onClick={onClose} aria-label="Hide panel">
          <PanelRightClose />
        </Button>
      </div>

      {!task ? (
        <div className="flex-1" />
      ) : (
        <ScrollArea className="flex-1 px-4 pb-6">
          <Section title="Input">
            {inputs.length === 0 ? (
              <Empty>No input files</Empty>
            ) : (
              <FileList>
                {inputs.map((f) => (
                  <FileRow
                    key={f.path}
                    label={f.name}
                    size={f.size}
                    onClick={() => onOpenFile(f)}
                  />
                ))}
              </FileList>
            )}
          </Section>

          <Section title="Output">
            {outputs.length === 0 ? (
              <Empty>No output files</Empty>
            ) : (
              <FileList>
                {outputs.map((f) => (
                  <FileRow
                    key={f.path}
                    label={f.name}
                    size={f.size}
                    onClick={() => onOpenFile(f)}
                  />
                ))}
              </FileList>
            )}
          </Section>
        </ScrollArea>
      )}
    </aside>
  );
}


function Section({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div className="pt-4">
      <h3 className="mb-1.5 text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {title}
      </h3>
      {children}
    </div>
  );
}

function FileList({ children }: { children: React.ReactNode }) {
  return <ul className="space-y-1">{children}</ul>;
}

function FileRow({
  label,
  size,
  onClick,
}: {
  label: string;
  size?: number;
  onClick?: () => void;
}) {
  return (
    <li>
      <button
        type="button"
        onClick={onClick}
        className="flex w-full items-center gap-2 rounded-md border bg-background px-2 py-1.5 text-left text-xs transition-colors hover:bg-accent"
      >
        <FileText className="size-3 shrink-0 text-muted-foreground" />
        <span className="truncate font-mono">{label}</span>
        {size !== undefined && (
          <span className="ml-auto shrink-0 text-muted-foreground">
            {formatBytes(size)}
          </span>
        )}
      </button>
    </li>
  );
}

function Empty({ children }: { children: React.ReactNode }) {
  return <p className="text-xs text-muted-foreground">{children}</p>;
}
