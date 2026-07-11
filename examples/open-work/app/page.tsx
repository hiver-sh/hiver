"use client";

import * as React from "react";
import { PanelLeft, PanelRight, Settings } from "lucide-react";

import {
  BrowserThumbnail,
  BrowserViewer,
  useBrowserStream,
} from "@/components/browser-viewer";
import { Conversation } from "@/components/conversation";
import { FileViewer } from "@/components/file-viewer";
import { IoPanel } from "@/components/io-panel";
import { ModeToggle } from "@/components/mode-toggle";
import { PromptInput } from "@/components/prompt-input";
import { SettingsPanel } from "@/components/settings-panel";
import { TaskSidebar } from "@/components/task-sidebar";
import { Button } from "@/components/ui/button";
import {
  isOrchestration,
  ORCHESTRATION_ORDER,
  ORCHESTRATIONS,
  orchestrationForModel,
  type Orchestration,
} from "@/lib/orchestration";
import { buildFolder, supportsFolderPicker, type FolderState } from "@/lib/folder";
import { mention, mentionPath, mentionRegex } from "@/lib/mention";
import { systemHint } from "@/lib/system-message";
import {
  applyEvent,
  applyTaskEvent,
  clearPendingEgress,
  createTask,
  createTurn,
  messagesToTurns,
  taskTitle,
  type Task,
} from "@/lib/task";
import {
  allowEgress,
  createTask as createTaskId,
  deleteTaskSandbox,
  fetchConversation,
  fetchFiles,
  openStream,
  sendTask,
} from "@/lib/task-client";
import type { Attachment, ManifestEntry, OutputFile } from "@/lib/types";

const KEYS_STORE = "openwork:apiKeys";
const ORCH_STORE = "openwork:orchestration";
const MODELS_STORE = "openwork:models";
const TASKS_STORE = "openwork:tasks";
const TTL_STORE = "openwork:ttlMinutes";
const GATEWAY_STORE = "openwork:gatewayUrl";
const DEFAULT_TTL_MINUTES = 30;
const DEFAULT_GATEWAY = "http://localhost:10000";
// Quiet period after the last egress approval before telling the agent access
// was granted (approving often triggers a reload that requests more hosts).
const GRANT_NOTICE_IDLE_MS = 1000;

function loadGatewayUrl(): string {
  if (typeof window === "undefined") return DEFAULT_GATEWAY;
  return localStorage.getItem(GATEWAY_STORE) || DEFAULT_GATEWAY;
}

function loadTtlMinutes(): number {
  if (typeof window === "undefined") return DEFAULT_TTL_MINUTES;
  const raw = localStorage.getItem(TTL_STORE);
  if (raw === null) return DEFAULT_TTL_MINUTES;
  const n = Number(raw);
  return Number.isFinite(n) && n >= 0 ? n : DEFAULT_TTL_MINUTES;
}

/** Read the active task id from the /task/<id> URL. */
function idFromPath(path: string): string | null {
  const m = path.match(/^\/task\/([^/?#]+)/);
  return m ? m[1] : null;
}

/** Restore the thread list (metadata only; turns come from the sandbox). */
function loadTaskMetas(): Task[] {
  if (typeof window === "undefined") return [];
  try {
    const raw = localStorage.getItem(TASKS_STORE);
    const metas = raw
      ? (JSON.parse(raw) as { id: string; title: string; createdAt: number }[])
      : [];
    return metas.map((m) => ({
      ...m,
      turns: [],
      inputs: [],
      outputs: [],
      pendingEgress: [],
      browser: false,
    }));
  } catch {
    return [];
  }
}


function loadApiKeys(): Record<Orchestration, string> {
  const empty = { claude: "", codex: "" };
  if (typeof window === "undefined") return empty;
  try {
    const raw = localStorage.getItem(KEYS_STORE);
    return raw ? { ...empty, ...JSON.parse(raw) } : empty;
  } catch {
    return empty;
  }
}

function loadOrchestration(): Orchestration {
  if (typeof window === "undefined") return "claude";
  const stored = localStorage.getItem(ORCH_STORE) ?? "";
  return isOrchestration(stored) ? stored : "claude";
}

function loadModels(): Record<Orchestration, string> {
  const def = {
    claude: ORCHESTRATIONS.claude.defaultModel,
    codex: ORCHESTRATIONS.codex.defaultModel,
  };
  if (typeof window === "undefined") return def;
  try {
    const raw = localStorage.getItem(MODELS_STORE);
    const stored = raw ? (JSON.parse(raw) as Partial<Record<Orchestration, string>>) : {};
    // Keep a stored model only if it's still a valid id for that engine.
    const valid = (o: Orchestration) =>
      ORCHESTRATIONS[o].models.some((m) => m.id === stored[o])
        ? (stored[o] as string)
        : def[o];
    return { claude: valid("claude"), codex: valid("codex") };
  } catch {
    return def;
  }
}

export default function Page() {
  // Persisted state starts at SSR-safe defaults and is loaded from localStorage
  // after mount (see the hydrate effect), so the first client render matches the
  // server and there's no hydration mismatch.
  const [hydrated, setHydrated] = React.useState(false);
  const [tasks, setTasks] = React.useState<Task[]>([]);
  // Active task is driven by the /task/<id> URL, kept in state via pushState.
  const [activeId, setActiveId] = React.useState<string | null>(null);
  const restored = React.useRef<Set<string>>(new Set());

  // Client-side navigation between "/" and "/task/<id>" without a remount.
  const navigate = React.useCallback((id: string | null) => {
    const url = id ? `/task/${id}` : "/";
    window.history.pushState({}, "", url);
    setActiveId(id);
    setViewerFile(null); // close any open file when changing tasks
    setBrowserOpen(false);
  }, []);
  const [prompt, setPrompt] = React.useState("");
  const [attachments, setAttachments] = React.useState<Attachment[]>([]);
  const [folder, setFolder] = React.useState<FolderState | null>(null);
  const [panelOpen, setPanelOpen] = React.useState(true);
  const [sidebarOpen, setSidebarOpen] = React.useState(true);
  const [settingsOpen, setSettingsOpen] = React.useState(false);
  const [orchestration, setOrchestration] = React.useState<Orchestration>("claude");
  const [apiKeys, setApiKeys] = React.useState<Record<Orchestration, string>>({
    claude: "",
    codex: "",
  });
  const [models, setModels] = React.useState<Record<Orchestration, string>>({
    claude: ORCHESTRATIONS.claude.defaultModel,
    codex: ORCHESTRATIONS.codex.defaultModel,
  });
  const [ttlMinutes, setTtlMinutes] = React.useState(DEFAULT_TTL_MINUTES);
  const [gatewayUrl, setGatewayUrl] = React.useState(DEFAULT_GATEWAY);
  const [viewerFile, setViewerFile] = React.useState<OutputFile | null>(null);
  const [viewerReload, setViewerReload] = React.useState(0);
  const [browserOpen, setBrowserOpen] = React.useState(false);
  const [folderSupported, setFolderSupported] = React.useState(false);

  React.useEffect(() => setFolderSupported(supportsFolderPicker()), []);

  // Load persisted state + the URL's active task after mount (client-only) to
  // avoid a hydration mismatch.
  React.useEffect(() => {
    setTasks(loadTaskMetas());
    setActiveId(idFromPath(window.location.pathname));
    setOrchestration(loadOrchestration());
    setApiKeys(loadApiKeys());
    setModels(loadModels());
    setTtlMinutes(loadTtlMinutes());
    setGatewayUrl(loadGatewayUrl());
    setHydrated(true);
  }, []);

  // Back/forward buttons update the active task.
  React.useEffect(() => {
    const onPop = () => setActiveId(idFromPath(window.location.pathname));
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  // Persist preferences + the thread list — but only after hydration, so the
  // initial defaults never clobber stored values.
  React.useEffect(() => {
    if (!hydrated) return;
    try {
      localStorage.setItem(KEYS_STORE, JSON.stringify(apiKeys));
    } catch {
      /* storage unavailable */
    }
  }, [apiKeys, hydrated]);
  React.useEffect(() => {
    if (!hydrated) return;
    try {
      localStorage.setItem(ORCH_STORE, orchestration);
    } catch {
      /* storage unavailable */
    }
  }, [orchestration, hydrated]);
  React.useEffect(() => {
    if (!hydrated) return;
    try {
      localStorage.setItem(MODELS_STORE, JSON.stringify(models));
    } catch {
      /* storage unavailable */
    }
  }, [models, hydrated]);
  React.useEffect(() => {
    if (!hydrated) return;
    try {
      localStorage.setItem(TTL_STORE, String(ttlMinutes));
    } catch {
      /* storage unavailable */
    }
  }, [ttlMinutes, hydrated]);
  React.useEffect(() => {
    if (!hydrated) return;
    try {
      localStorage.setItem(GATEWAY_STORE, gatewayUrl);
    } catch {
      /* storage unavailable */
    }
  }, [gatewayUrl, hydrated]);
  React.useEffect(() => {
    if (!hydrated) return;
    try {
      const metas = tasks.map((t) => ({
        id: t.id,
        title: t.title,
        createdAt: t.createdAt,
      }));
      localStorage.setItem(TASKS_STORE, JSON.stringify(metas));
    } catch {
      /* storage unavailable */
    }
  }, [tasks, hydrated]);

  // Refs so the SSE handler (mounted once) sees the current open file.
  const viewerFileRef = React.useRef(viewerFile);
  viewerFileRef.current = viewerFile;
  const activeIdRef = React.useRef(activeId);
  activeIdRef.current = activeId;
  // Latest tasks, so the debounced grant notice can check pending egress.
  const tasksRef = React.useRef(tasks);
  tasksRef.current = tasks;
  // Idle-debounced "access granted" notice: `id` is the task awaiting it.
  const grantNotice = React.useRef<{
    timer: ReturnType<typeof setTimeout> | null;
    id: string | null;
  }>({ timer: null, id: null });

  // Single shared SSE stream: route each event to its task's latest turn.
  React.useEffect(() => {
    return openStream(({ session, event }) => {
      // Keep the open file in sync: reload on overwrite, close on delete.
      if (session === activeIdRef.current && viewerFileRef.current) {
        if (event.type === "file" && event.file.path === viewerFileRef.current.path) {
          setViewerReload((k) => k + 1);
        } else if (
          event.type === "file-removed" &&
          event.path === viewerFileRef.current.path
        ) {
          setViewerFile(null);
        }
      }

      setTasks((prev) =>
        prev.map((t) => {
          if (t.id !== session) return t;
          // Files + egress + browser are task-level (persist across turns).
          if (
            event.type === "file" ||
            event.type === "file-removed" ||
            event.type === "egress-denied" ||
            event.type === "browser"
          ) {
            return applyTaskEvent(t, event);
          }
          if (t.turns.length === 0) return t;
          const last = t.turns.length - 1;
          return {
            ...t,
            turns: t.turns.map((tn, i) =>
              i === last ? applyEvent(tn, event) : tn,
            ),
          };
        }),
      );
    });
  }, []);

  // When a thread is opened and its turns aren't loaded, restore them from the
  // sandbox transcript so a refresh doesn't lose the conversation.
  React.useEffect(() => {
    if (!activeId || restored.current.has(activeId)) return;
    const task = tasks.find((t) => t.id === activeId);
    if (!task) return;
    restored.current.add(activeId);
    if (task.turns.length > 0) return; // live thread, nothing to restore
    Promise.all([
      fetchConversation(activeId, gatewayUrl),
      fetchFiles(activeId, gatewayUrl),
    ]).then(
      ([messages, files]) => {
        if (
          messages.length === 0 &&
          files.inputs.length === 0 &&
          files.outputs.length === 0
        )
          return;
        setTasks((prev) =>
          prev.map((t) =>
            t.id === activeId && t.turns.length === 0
              ? {
                  ...t,
                  turns: messagesToTurns(messages),
                  inputs: files.inputs,
                  outputs: files.outputs,
                }
              : t,
          ),
        );
      },
    );
  }, [activeId, tasks, gatewayUrl]);

  // Start with the drawers collapsed on small screens; open on desktop.
  React.useEffect(() => {
    if (window.matchMedia("(max-width: 1023px)").matches) {
      setSidebarOpen(false);
      setPanelOpen(false);
    }
  }, []);

  // Cancel a pending grant notice when switching away from its task.
  React.useEffect(() => {
    return () => {
      const g = grantNotice.current;
      if (g.timer) clearTimeout(g.timer);
      g.timer = null;
      g.id = null;
    };
  }, [activeId]);

  const activeTask = tasks.find((t) => t.id === activeId) ?? null;
  const busy = tasks.some((t) =>
    t.turns.some((turn) => turn.status === "streaming"),
  );

  // Auto-open the browser viewer the first time a task's browser VM appears.
  const browserReady = activeTask?.browser ?? false;
  // Stream the browser while it's available (open or minimized) so the thumbnail
  // and its URL stay live.
  const browserStream = useBrowserStream(activeId, gatewayUrl, browserReady);
  React.useEffect(() => {
    if (browserReady) setBrowserOpen(true);
  }, [browserReady, activeId]);

  // Sandbox files the active task can `@`-reference (already in /workspace).
  const sandboxFiles = React.useMemo(() => {
    if (!activeTask) return [];
    return [...activeTask.inputs, ...activeTask.outputs].map((f) => ({
      relPath: f.path.replace(/^\/workspace\//, ""),
      name: f.name,
    }));
  }, [activeTask]);

  function newTask() {
    navigate(null);
    setPrompt("");
    setAttachments([]);
  }

  function deleteTask(id: string) {
    setTasks((prev) => prev.filter((t) => t.id !== id));
    restored.current.delete(id);
    if (activeId === id) navigate(null);
    void deleteTaskSandbox(id); // shut the sandbox down server-side
  }

  function clearEgress(taskId: string, host: string) {
    setTasks((prev) =>
      prev.map((t) => (t.id === taskId ? clearPendingEgress(t, host) : t)),
    );
  }
  async function handleAllowEgress(hosts: string[]) {
    const id = activeId;
    if (!id || hosts.length === 0) return;
    await Promise.all(hosts.map((host) => allowEgress(id, host, gatewayUrl)));
    hosts.forEach((host) => clearEgress(id, host));
    // Approving often triggers a reload that requests more hosts. Rather than
    // nudge per approval, wait until the requests settle, then tell the agent
    // once — generically, without naming hosts.
    scheduleGrantNotice(id);
  }

  /** After the egress prompts go quiet, send the agent a single generic notice. */
  function scheduleGrantNotice(id: string) {
    const g = grantNotice.current;
    g.id = id;
    if (g.timer) clearTimeout(g.timer);
    g.timer = setTimeout(() => {
      g.timer = null;
      // Still awaiting approvals → let it settle further.
      const task = tasksRef.current.find((t) => t.id === id);
      if (task && task.pendingEgress.length > 0) {
        scheduleGrantNotice(id);
        return;
      }
      if (g.id !== id) return;
      g.id = null;
      void sendSystemHint(
        id,
        "Network access has been granted. You can retry now if a request was blocked.",
      );
    }, GRANT_NOTICE_IDLE_MS);
  }

  /** Inject a hidden system hint into the agent's conversation. We deliberately
   * don't create a turn — that would steal the SSE's "route to last turn" target
   * and strand the in-flight turn as streaming. Claude's reply, if any, simply
   * flows into the current last turn. */
  async function sendSystemHint(id: string, text: string) {
    const form = new FormData();
    form.set("sessionId", id);
    form.set("prompt", systemHint(text));
    form.set("orchestration", orchestration);
    form.set("model", models[orchestration]);
    form.set("apiKey", apiKeys[orchestration]);
    form.set("ttl", String(ttlMinutes * 60));
    form.set("gatewayUrl", gatewayUrl);
    form.set("manifest", "[]");
    try {
      await sendTask(form); // best-effort; nothing to show if it fails
    } catch {
      /* ignore */
    }
  }
  function handleDismissEgress(hosts: string[]) {
    if (!activeId) return;
    for (const host of hosts) clearEgress(activeId, host);
  }

  /** Picking a model also selects its engine — and thus the sandbox image, which
   * the server switches to on the next send. */
  function handleModelChange(modelId: string) {
    const orch = orchestrationForModel(modelId);
    if (!orch) return;
    setOrchestration(orch);
    setModels((prev) => ({ ...prev, [orch]: modelId }));
  }

  function handlePickFolderFiles(files: FileList) {
    const picked = buildFolder(files);
    if (picked) setFolder(picked);
  }

  async function handleSubmit() {
    const text = prompt.trim();
    if (!text || busy) return;

    // Files whose `@relPath` token appears in the prompt (quoted or not).
    const tokens = new Set(
      Array.from(text.matchAll(mentionRegex())).map(mentionPath),
    );
    const refs = (folder?.entries ?? []).filter((e) => tokens.has(e.relPath));

    const form = new FormData();
    const manifest: ManifestEntry[] = [];
    const used = new Set<string>();
    const uniqueName = (base: string) => {
      let name = base;
      let n = 1;
      while (used.has(name)) {
        const dot = base.lastIndexOf(".");
        name =
          dot > 0
            ? `${base.slice(0, dot)}-${n}${base.slice(dot)}`
            : `${base}-${n}`;
        n++;
      }
      used.add(name);
      return name;
    };

    // Referenced folder files upload to /workspace/input; remember the uploaded
    // name per token so we can point the `@` reference at its real location.
    const refNames = new Map<string, string>();
    for (const ref of refs) {
      const file = await ref.getFile();
      const name = uniqueName(ref.name);
      form.append("file", file, name);
      manifest.push({ name, relPath: ref.relPath, kind: "reference", size: file.size });
      refNames.set(ref.relPath, name);
    }
    for (const att of attachments) {
      const name = uniqueName(att.file.name);
      form.append("file", att.file, name);
      manifest.push({
        name,
        relPath: att.file.name,
        kind: "attachment",
        size: att.file.size,
      });
    }
    form.set("manifest", JSON.stringify(manifest));

    // Rewrite `@<folderPath>` tokens to `@input/<uploadedName>` (where the file
    // actually landed), re-quoting since uploaded names may contain spaces.
    // Sandbox `@input/…`/`@output/…` tokens are left as-is.
    const resolvedText = text.replace(mentionRegex(), (m, quoted, bare) => {
      const tok = quoted ?? bare;
      return refNames.has(tok) ? mention(`input/${refNames.get(tok)}`) : m;
    });
    form.set("prompt", resolvedText);

    // Selected engine, model, and its API key (injected via egress override).
    form.set("orchestration", orchestration);
    form.set("model", models[orchestration]);
    form.set("apiKey", apiKeys[orchestration]);
    form.set("ttl", String(ttlMinutes * 60));
    form.set("gatewayUrl", gatewayUrl);

    // Append this turn to the active thread, or start a new one. New tasks get
    // a server-minted id (their sandbox key); we navigate to /task/<id>.
    const turn = createTurn(resolvedText, manifest);
    let taskId = activeTask?.id;
    if (taskId) {
      const id = taskId;
      setTasks((prev) =>
        prev.map((t) =>
          t.id === id ? { ...t, turns: [...t.turns, turn] } : t,
        ),
      );
    } else {
      taskId = await createTaskId();
      restored.current.add(taskId); // brand new — nothing to restore
      setTasks((prev) => [createTask(taskId!, taskTitle(text), turn), ...prev]);
      navigate(taskId);
    }

    // The task id is the session id (and sandbox key) — the server keeps one
    // agent process per task and feeds each turn to it over stdin.
    form.set("sessionId", taskId);

    setPrompt("");
    setAttachments([]);
    setPanelOpen(true);

    // Fire-and-forget: the turn's output arrives over the shared SSE stream and
    // is routed to this task's latest turn.
    try {
      await sendTask(form);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setTasks((prev) =>
        prev.map((t) =>
          t.id === taskId
            ? {
                ...t,
                turns: t.turns.map((tn) =>
                  tn.id === turn.id ? applyEvent(tn, { type: "error", message }) : tn,
                ),
              }
            : t,
        ),
      );
    }
  }

  return (
    <div className="flex h-dvh overflow-hidden bg-background">
      {sidebarOpen && (
        <>
          <div
            className="fixed inset-0 z-30 bg-black/50 lg:hidden"
            onClick={() => setSidebarOpen(false)}
            aria-hidden
          />
          <TaskSidebar
            tasks={tasks}
            activeId={activeId}
            onSelect={(id) => {
              navigate(id);
              if (window.matchMedia("(max-width: 1023px)").matches) {
                setSidebarOpen(false);
              }
            }}
            onNew={newTask}
            onDelete={deleteTask}
            onCollapse={() => setSidebarOpen(false)}
          />
        </>
      )}

      <main className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-14 shrink-0 items-center justify-between gap-2 border-b px-4">
          <div className="flex min-w-0 items-center gap-2">
            {!sidebarOpen && (
              <Button
                variant="ghost"
                size="icon-sm"
                onClick={() => setSidebarOpen(true)}
                aria-label="Show sidebar"
              >
                <PanelLeft />
              </Button>
            )}
            <h1 className="truncate text-sm font-medium">
              {activeTask ? activeTask.title : "New task"}
            </h1>
          </div>
          <div className="flex items-center gap-1">
            {!panelOpen && (
              <Button
                variant="ghost"
                size="icon-sm"
                onClick={() => setPanelOpen(true)}
                aria-label="Show workspace"
              >
                <PanelRight />
              </Button>
            )}
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label="Settings"
              onClick={() => setSettingsOpen(true)}
            >
              <Settings />
            </Button>
            <ModeToggle />
          </div>
        </header>

        <div className="flex min-h-0 flex-1 flex-col">
          <Conversation
            task={activeTask}
            onAllowEgress={handleAllowEgress}
            onDismissEgress={handleDismissEgress}
          />
          <div className="bg-background/60 px-4 py-3">
            <div className="mx-auto max-w-3xl space-y-2">
              {browserReady && !browserOpen && (
                <BrowserThumbnail
                  frame={browserStream.frame}
                  meta={browserStream.meta}
                  onClick={() => setBrowserOpen(true)}
                />
              )}
              <PromptInput
                value={prompt}
                onChange={setPrompt}
                folder={folder}
                attachments={attachments}
                disabled={busy}
                folderSupported={folderSupported}
                sandboxFiles={sandboxFiles}
                models={ORCHESTRATION_ORDER.flatMap((o) => ORCHESTRATIONS[o].models)}
                model={models[orchestration]}
                onModelChange={handleModelChange}
                onSubmit={handleSubmit}
                onPickFolderFiles={handlePickFolderFiles}
                onClearFolder={() => setFolder(null)}
                onAddAttachments={(files) =>
                  setAttachments((prev) => [
                    ...prev,
                    ...files.map((file) => ({
                      id: crypto.randomUUID(),
                      file,
                    })),
                  ])
                }
                onRemoveAttachment={(id) =>
                  setAttachments((prev) => prev.filter((a) => a.id !== id))
                }
              />
            </div>
          </div>
        </div>
      </main>

      {/* File viewer opens as its own column, condensing the chat. */}
      {viewerFile && activeId && (
        <FileViewer
          key={viewerFile.path}
          session={activeId}
          file={viewerFile}
          gatewayUrl={gatewayUrl}
          reloadKey={viewerReload}
          onClose={() => setViewerFile(null)}
        />
      )}

      {/* Browser viewer opens as its own column; minimizes to a thumbnail. */}
      {browserOpen && activeId && browserReady && (
        <BrowserViewer
          session={activeId}
          gatewayUrl={gatewayUrl}
          frame={browserStream.frame}
          meta={browserStream.meta}
          status={browserStream.status}
          onMinimize={() => setBrowserOpen(false)}
        />
      )}

      {panelOpen && (
        <>
          <div
            className="fixed inset-0 z-30 bg-black/50 lg:hidden"
            onClick={() => setPanelOpen(false)}
            aria-hidden
          />
          <IoPanel
            task={activeTask}
            onOpenFile={setViewerFile}
            onClose={() => setPanelOpen(false)}
          />
        </>
      )}

      <SettingsPanel
        open={settingsOpen}
        onClose={() => setSettingsOpen(false)}
        sandboxKey={activeId}
        ttlMinutes={ttlMinutes}
        onTtlChange={setTtlMinutes}
        gatewayUrl={gatewayUrl}
        onGatewayUrlChange={setGatewayUrl}
        apiKeys={apiKeys}
        onApiKeyChange={(o, key) =>
          setApiKeys((prev) => ({ ...prev, [o]: key }))
        }
      />
    </div>
  );
}
