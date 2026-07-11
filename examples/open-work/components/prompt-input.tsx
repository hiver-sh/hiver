"use client";

import * as React from "react";
import {
  ArrowUp,
  AtSign,
  File as FileIcon,
  Folder,
  FolderPlus,
  Loader2,
  Paperclip,
  X,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { cn, formatBytes } from "@/lib/utils";
import type { FolderState } from "@/lib/folder";
import { mention as formatMention } from "@/lib/mention";
import type { Attachment } from "@/lib/types";

export function PromptInput({
  value,
  onChange,
  folder,
  attachments,
  disabled,
  folderSupported,
  sandboxFiles,
  models,
  model,
  onModelChange,
  onSubmit,
  onPickFolderFiles,
  onClearFolder,
  onAddAttachments,
  onRemoveAttachment,
}: {
  value: string;
  onChange: (v: string) => void;
  folder: FolderState | null;
  attachments: Attachment[];
  disabled: boolean;
  folderSupported: boolean;
  /** Files already in the active task's sandbox (/workspace/input|output). */
  sandboxFiles: { relPath: string; name: string }[];
  /** All selectable models across providers; picking one switches the engine + image. */
  models: { id: string; label: string }[];
  model: string;
  onModelChange: (model: string) => void;
  onSubmit: () => void;
  onPickFolderFiles: (files: FileList) => void;
  onClearFolder: () => void;
  onAddAttachments: (files: File[]) => void;
  onRemoveAttachment: (id: string) => void;
}) {
  const taRef = React.useRef<HTMLTextAreaElement>(null);
  const fileRef = React.useRef<HTMLInputElement>(null);
  const folderRef = React.useRef<HTMLInputElement>(null);
  const modelRef = React.useRef<HTMLSelectElement>(null);
  const modelMeasureRef = React.useRef<HTMLSpanElement>(null);
  const [mention, setMention] = React.useState<{ query: string; start: number } | null>(null);
  const [activeIdx, setActiveIdx] = React.useState(0);

  // Size the model select to fit the selected label (not the widest option) —
  // measured via a hidden mirror span using the same font, since a native
  // <select>'s closed-box width isn't reliably tied to just the selection.
  React.useLayoutEffect(() => {
    const select = modelRef.current;
    const measure = modelMeasureRef.current;
    if (!select || !measure) return;
    select.style.width = `${measure.offsetWidth}px`;
  }, [model, models]);

  // Everything `@` can reference: the sandbox's input/output files plus any
  // local folder the user granted.
  const mentionables = React.useMemo<{ relPath: string; name: string }[]>(
    () => [
      ...sandboxFiles,
      ...(folder?.entries ?? []).map((e) => ({ relPath: e.relPath, name: e.name })),
    ],
    [sandboxFiles, folder],
  );

  const matches = React.useMemo(() => {
    if (!mention) return [];
    const q = mention.query.toLowerCase();
    if (!q) return mentionables.slice(0, 50);
    // Rank: filename prefix > filename contains > path contains.
    const score = (e: { relPath: string; name: string }) => {
      const name = e.name.toLowerCase();
      const path = e.relPath.toLowerCase();
      if (name.startsWith(q)) return 0;
      if (name.includes(q)) return 1;
      if (path.includes(q)) return 2;
      return 3;
    };
    return mentionables
      .map((e) => ({ e, s: score(e) }))
      .filter((x) => x.s < 3)
      .sort((a, b) => a.s - b.s || a.e.relPath.localeCompare(b.e.relPath))
      .slice(0, 50)
      .map((x) => x.e);
  }, [mention, mentionables]);

  // Grow the textarea with its content, up to a cap.
  React.useEffect(() => {
    const ta = taRef.current;
    if (!ta) return;
    ta.style.height = "auto";
    ta.style.height = `${Math.min(ta.scrollHeight, 180)}px`;
  }, [value]);

  // Keep focus on the composer: on mount, after each send clears the value,
  // and whenever a run finishes (disabled flips back to false).
  React.useEffect(() => {
    if (!disabled) taRef.current?.focus();
  }, [disabled, value === ""]);

  function updateMention(v: string, caret: number) {
    const upto = v.slice(0, caret);
    const m = upto.match(/(^|\s)@([^\s@]*)$/);
    if (m && mentionables.length) {
      setMention({ query: m[2], start: caret - m[2].length - 1 });
      setActiveIdx(0);
    } else {
      setMention(null);
    }
  }

  function selectEntry(entry: { relPath: string }) {
    const ta = taRef.current;
    if (!ta || !mention) return;
    const caret = ta.selectionStart;
    const before = value.slice(0, mention.start);
    const after = value.slice(caret);
    const insert = `${formatMention(entry.relPath)} `;
    onChange(before + insert + after);
    setMention(null);
    requestAnimationFrame(() => {
      const pos = (before + insert).length;
      ta.focus();
      ta.setSelectionRange(pos, pos);
    });
  }

  function handleKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (mention && matches.length) {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setActiveIdx((i) => (i + 1) % matches.length);
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        setActiveIdx((i) => (i - 1 + matches.length) % matches.length);
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        selectEntry(matches[activeIdx]);
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        setMention(null);
        return;
      }
    }
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      onSubmit();
    }
  }

  const canSend = value.trim().length > 0 && !disabled;

  return (
    <div className="relative rounded-3xl border border-border/60 bg-muted shadow-sm transition-shadow focus-within:shadow-md">
      {/* @-mention menu, floating above the input */}
      {mention && matches.length > 0 && (
        <div className="absolute bottom-full left-2 z-20 mb-2 w-[min(24rem,80vw)] overflow-hidden rounded-lg border bg-popover bg-card shadow-lg">
          <div className="flex items-center gap-1.5 border-b px-3 py-1.5 text-xs text-muted-foreground">
            <AtSign className="size-3" />
            Reference a file
          </div>
          <ul className="max-h-64 overflow-y-auto scroll-slim py-1">
            {matches.map((entry, i) => (
              <li key={entry.relPath}>
                <button
                  type="button"
                  onMouseDown={(e) => {
                    e.preventDefault();
                    selectEntry(entry);
                  }}
                  onMouseEnter={() => setActiveIdx(i)}
                  className={cn(
                    "flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm",
                    i === activeIdx ? "bg-accent" : "hover:bg-accent/60",
                  )}
                >
                  <FileIcon className="size-3.5 shrink-0 text-muted-foreground" />
                  <span className="truncate">{entry.relPath}</span>
                </button>
              </li>
            ))}
          </ul>
        </div>
      )}

      {/* Attachment + folder chips */}
      {(attachments.length > 0 || folder) && (
        <div className="flex flex-wrap gap-1.5 px-5 pt-4">
          {folder && (
            <Badge variant="secondary" className="gap-1.5">
              <Folder className="size-3" />
              {folder.name}
              <span className="text-muted-foreground">· {folder.entries.length} files</span>
              <button
                type="button"
                onClick={onClearFolder}
                className="ml-0.5 rounded-sm opacity-70 hover:opacity-100"
                aria-label="Remove folder"
              >
                <X className="size-3" />
              </button>
            </Badge>
          )}
          {attachments.map((a) => (
            <Badge key={a.id} variant="muted" className="gap-1.5">
              <Paperclip className="size-3" />
              <span className="max-w-[12rem] truncate">{a.file.name}</span>
              <span className="text-muted-foreground/70">{formatBytes(a.file.size)}</span>
              <button
                type="button"
                onClick={() => onRemoveAttachment(a.id)}
                className="ml-0.5 rounded-sm opacity-70 hover:opacity-100"
                aria-label={`Remove ${a.file.name}`}
              >
                <X className="size-3" />
              </button>
            </Badge>
          ))}
        </div>
      )}

      <Textarea
        ref={taRef}
        value={value}
        rows={1}
        placeholder="Describe a task… use @ to reference a file from your folder"
        autoFocus
        // Stays focusable while a run streams; Enter-to-send is gated by `disabled`.
        onChange={(e) => {
          onChange(e.target.value);
          updateMention(e.target.value, e.target.selectionStart);
        }}
        onKeyDown={handleKeyDown}
        onClick={(e) => updateMention(value, e.currentTarget.selectionStart)}
        className="max-h-[180px] px-5 pt-4"
      />

      <div className="flex items-center justify-between gap-2 px-3 pb-3 pt-1">
        <div className="flex items-center gap-1">
          <input
            ref={fileRef}
            type="file"
            multiple
            hidden
            onChange={(e) => {
              const list = Array.from(e.target.files ?? []);
              if (list.length) onAddAttachments(list);
              e.target.value = "";
            }}
          />
          <input
            ref={folderRef}
            type="file"
            hidden
            // `webkitdirectory` turns this into a folder picker; not in React's
            // input typings, so spread it in.
            {...({ webkitdirectory: "", directory: "" } as Record<string, string>)}
            onChange={(e) => {
              if (e.target.files && e.target.files.length) {
                onPickFolderFiles(e.target.files);
              }
              e.target.value = "";
            }}
          />
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="rounded-full text-muted-foreground"
            onClick={() => fileRef.current?.click()}
          >
            <Paperclip />
            Attach
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="rounded-full text-muted-foreground"
            disabled={!folderSupported}
            title={
              folderSupported
                ? "Pick a local folder to reference files from"
                : "Folder access isn't supported in this browser"
            }
            onClick={() => folderRef.current?.click()}
          >
            <FolderPlus />
            {folder ? "Change folder" : "Add folder"}
          </Button>

          <select
            ref={modelRef}
            value={model}
            onChange={(e) => onModelChange(e.target.value)}
            aria-label="Model"
            className="h-8 shrink-0 cursor-pointer rounded-full border-0 bg-transparent px-2 text-xs font-medium text-muted-foreground hover:bg-accent focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
          >
            {models.map((m) => (
              <option key={m.id} value={m.id}>
                {m.label}
              </option>
            ))}
          </select>
          {/* Hidden mirror of the selected label, same font/padding as the
           * select, used only to measure its natural width. */}
          <span
            ref={modelMeasureRef}
            aria-hidden
            className="pointer-events-none absolute -z-10 whitespace-pre px-2 text-xs font-medium opacity-0"
            style={{ left: -9999 }}
          >
            {models.find((m) => m.id === model)?.label ?? ""}
            {/* Room for the native dropdown arrow. */}
            {"    "}
          </span>
        </div>

        <Button
          type="button"
          size="icon"
          className="rounded-full"
          disabled={!canSend}
          onClick={onSubmit}
        >
          {disabled ? <Loader2 className="animate-spin" /> : <ArrowUp />}
          <span className="sr-only">Send</span>
        </Button>
      </div>
    </div>
  );
}
