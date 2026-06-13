import {
  ChevronDown,
  ChevronRight,
  Cloud,
  File,
  Folder,
  FolderOpen,
  Loader2,
  RefreshCw,
} from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { cn } from "@/lib/utils";
import { CodeViewer, CODE_DIALOG_CLASS } from "@/components/CodeViewer";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import type { SandboxEvent } from "@/types";
import { langForPath } from "@/lib/fileUtils";
import { useTransport } from "@/lib/transport";
import { useUserPreferences } from "@/lib/userPreferences";

interface DirEntry {
  name: string;
  path: string;
  is_dir: boolean;
  size: number;
}

interface TreeNode extends DirEntry {
  children: TreeNode[] | null; // null = dir not yet fetched
  expanded: boolean;
  loading: boolean;
}

interface Preview {
  path: string;
  content: string;
  lang: string;
}

interface Props {
  sandboxKey: string;
  serverUrl: string;
  events: Extract<SandboxEvent, { type: "fs.request" }>[];
}

function updateNode(
  nodes: TreeNode[],
  path: string,
  fn: (n: TreeNode) => TreeNode,
): TreeNode[] {
  return nodes.map((n) => {
    if (n.path === path) return fn(n);
    if (n.children) return { ...n, children: updateNode(n.children, path, fn) };
    return n;
  });
}

function findNode(nodes: TreeNode[], path: string): TreeNode | null {
  for (const n of nodes) {
    if (n.path === path) return n;
    if (n.children) {
      const found = findNode(n.children, path);
      if (found) return found;
    }
  }
  return null;
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes}B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)}K`;
  return `${(bytes / 1024 / 1024).toFixed(1)}M`;
}

function toNodes(entries: DirEntry[]): TreeNode[] {
  return [...entries]
    .sort((a, b) => {
      if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
      return a.name.localeCompare(b.name);
    })
    .map((e) => ({ ...e, children: null, expanded: false, loading: false }));
}

function collectExpandedUnder(
  nodes: TreeNode[],
  mount: string,
  out: Set<string>,
) {
  for (const n of nodes) {
    if (!n.is_dir || !n.expanded || !n.children) continue;
    if (n.path === mount || n.path.startsWith(mount + "/")) out.add(n.path);
    collectExpandedUnder(n.children, mount, out);
  }
}

function mergeExpanded(newNodes: TreeNode[], oldNodes: TreeNode[]): TreeNode[] {
  return newNodes.map((newNode) => {
    const old = oldNodes.find((o) => o.path === newNode.path);
    if (!old || !old.expanded || !old.children) return newNode;
    return { ...newNode, expanded: true, children: old.children };
  });
}

export function FileExplorer({ sandboxKey, serverUrl, events }: Props) {
  const { transport } = useTransport();
  const { prefs, toggleExpandedPath } = useUserPreferences();
  const [roots, setRoots] = useState<TreeNode[]>([]);
  const [mountBackends, setMountBackends] = useState<Map<string, string>>(new Map());
  const [configLoading, setConfigLoading] = useState(true);
  const [configError, setConfigError] = useState<string | null>(null);
  const [preview, setPreview] = useState<Preview | null>(null);
  const [loadingPath, setLoadingPath] = useState<string | null>(null);
  const rootsRef = useRef<TreeNode[]>([]);
  const expandedPathsPrefRef = useRef<string[]>(
    prefs.expandedPaths[sandboxKey] ?? [],
  );
  const lastProcessedIdxRef = useRef(0);
  const pendingDirsRef = useRef<Set<string>>(new Set());
  const refreshTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    expandedPathsPrefRef.current = prefs.expandedPaths[sandboxKey] ?? [];
  }, [prefs.expandedPaths, sandboxKey]);

  const fileUrl = useCallback(
    (path: string) => {
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(sandboxKey)}/file`,
      );
      url.searchParams.set("path", path);

      return url;
    },
    [sandboxKey, serverUrl],
  );

  const fetchChildren = useCallback(
    async (path: string): Promise<TreeNode[]> => {
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(sandboxKey)}/directories`,
      );
      url.searchParams.set("path", path);

      console.log(
        "[FileExplorer] fetchChildren",
        path,
        "transport:",
        transport.constructor?.name ?? transport,
      );
      const data = await transport
        .fetch(url)
        .then((r) => r.json() as Promise<{ entries: DirEntry[] }>);
      return toNodes(data.entries);
    },
    [sandboxKey, serverUrl, transport],
  );

  const loadMounts = useCallback(async () => {
    setConfigLoading(true);
    setConfigError(null);
    try {
      const treeExpandedPaths: string[] = [];
      function collectExpanded(nodes: TreeNode[]) {
        for (const n of nodes) {
          if (n.expanded && n.children) {
            treeExpandedPaths.push(n.path);
            collectExpanded(n.children);
          }
        }
      }
      collectExpanded(rootsRef.current);

      const configUrl = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(sandboxKey)}/config`,
      );

      const allPaths = [
        "/",
        ...new Set([...treeExpandedPaths, ...expandedPathsPrefRef.current]),
      ];
      const [configResult, ...results] = await Promise.all([
        transport
          .fetch(configUrl)
          .then(
            (r) =>
              r.json() as Promise<{
                fs?: { mount: string; backend: string }[];
              }>,
          )
          .catch(() => null),
        ...allPaths.map((p) =>
          fetchChildren(p)
            .then((nodes) => ({ path: p, nodes }))
            .catch(() => null),
        ),
      ]);

      if (configResult?.fs) {
        setMountBackends(
          new Map(configResult.fs.map((f) => [f.mount, f.backend])),
        );
      }
      const freshByPath = new Map(
        results.filter((r) => r !== null).map((r) => [r.path, r.nodes]),
      );

      function buildTree(
        newNodes: TreeNode[],
        oldNodes: TreeNode[],
      ): TreeNode[] {
        return newNodes.map((newNode) => {
          const old = oldNodes.find((o) => o.path === newNode.path);
          const shouldExpand =
            old?.expanded ||
            expandedPathsPrefRef.current.includes(newNode.path);
          if (!shouldExpand) return newNode;
          const freshChildren = freshByPath.get(newNode.path);
          const children = freshChildren
            ? buildTree(freshChildren, old?.children ?? [])
            : (old?.children ?? null);
          return { ...newNode, expanded: !!children, children };
        });
      }

      const rootNodes = freshByPath.get("/") ?? rootsRef.current;
      setRoots((prev) => buildTree(rootNodes, prev));
    } catch (e) {
      setConfigError(String(e));
    } finally {
      setConfigLoading(false);
    }
  }, [fetchChildren]);

  useEffect(() => {
    loadMounts();
  }, [loadMounts]);

  useEffect(() => {
    rootsRef.current = roots;
  }, [roots]);

  useEffect(() => {
    // Reset index when events are cleared
    if (events.length < lastProcessedIdxRef.current) {
      lastProcessedIdxRef.current = 0;
    }
    if (events.length <= lastProcessedIdxRef.current) return;

    const newEvents = events.slice(lastProcessedIdxRef.current);
    lastProcessedIdxRef.current = events.length;

    for (const event of newEvents) {
      if (event.operation !== "write" || event.access !== "allowed") continue;
      collectExpandedUnder(
        rootsRef.current,
        event.mount,
        pendingDirsRef.current,
      );
    }

    if (pendingDirsRef.current.size === 0) return;

    if (refreshTimerRef.current) clearTimeout(refreshTimerRef.current);
    refreshTimerRef.current = setTimeout(() => {
      const dirs = new Set(pendingDirsRef.current);
      pendingDirsRef.current.clear();
      refreshTimerRef.current = null;

      for (const dir of dirs) {
        fetchChildren(dir)
          .then((children) => {
            setRoots((prev) =>
              updateNode(prev, dir, (n) => ({
                ...n,
                children: mergeExpanded(children, n.children ?? []),
              })),
            );
          })
          .catch(() => {});
      }
    }, 400);
  }, [events, fetchChildren]);

  async function toggle(path: string) {
    const node = findNode(roots, path);
    if (!node || !node.is_dir) return;

    if (node.expanded) {
      toggleExpandedPath(sandboxKey, path);
      setRoots((prev) =>
        updateNode(prev, path, (n) => ({ ...n, expanded: false })),
      );
      return;
    }
    toggleExpandedPath(sandboxKey, path);
    if (node.children !== null) {
      setRoots((prev) =>
        updateNode(prev, path, (n) => ({ ...n, expanded: true })),
      );
      return;
    }
    setRoots((prev) =>
      updateNode(prev, path, (n) => ({ ...n, loading: true })),
    );
    try {
      const children = await fetchChildren(path);
      setRoots((prev) =>
        updateNode(prev, path, (n) => ({
          ...n,
          loading: false,
          children,
          expanded: true,
        })),
      );
    } catch {
      setRoots((prev) =>
        updateNode(prev, path, (n) => ({ ...n, loading: false })),
      );
    }
  }

  function downloadFile(path: string) {
    const url = fileUrl(path);
    const a = document.createElement("a");
    a.href = url.toString();
    a.download = path.split("/").pop() ?? "file";
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
  }

  async function openFile(path: string) {
    const lang = langForPath(path);
    if (!lang) {
      downloadFile(path);
      return;
    }
    setLoadingPath(path);
    try {
      const content = await transport
        .fetch(fileUrl(path))
        .then((r) => r.text());
      setPreview({ path, content, lang });
    } catch {
      downloadFile(path);
    } finally {
      setLoadingPath(null);
    }
  }

  function renderNode(node: TreeNode, depth: number) {
    const isLoadingThis = loadingPath === node.path;
    const backend = mountBackends.get(node.path);
    const isCloudMount = node.is_dir && backend != null && backend !== "local";
    return (
      <div key={node.path}>
        <button
          className={cn(
            "flex w-full items-center gap-1.5 py-[3px] pr-3 text-xs text-left transition-colors hover:bg-muted/50",
            node.is_dir
              ? "text-foreground"
              : "text-muted-foreground hover:text-foreground",
          )}
          style={{ paddingLeft: 8 + depth * 14 }}
          onClick={() =>
            node.is_dir ? toggle(node.path) : openFile(node.path)
          }
          title={isCloudMount ? `${node.path} (${backend})` : node.path}
          disabled={isLoadingThis}
        >
          <span className="flex h-3.5 w-3.5 shrink-0 items-center justify-center">
            {node.is_dir &&
              (node.loading ? (
                <Loader2 className="h-3 w-3 animate-spin text-muted-foreground" />
              ) : node.expanded ? (
                <ChevronDown className="h-3 w-3 text-muted-foreground" />
              ) : (
                <ChevronRight className="h-3 w-3 text-muted-foreground" />
              ))}
            {!node.is_dir && isLoadingThis && (
              <Loader2 className="h-3 w-3 animate-spin text-muted-foreground" />
            )}
          </span>
          {node.is_dir ? (
            <>
              {node.expanded ? (
                <FolderOpen className="h-3.5 w-3.5 shrink-0 text-blue-600 dark:text-blue-400" />
              ) : (
                <Folder className="h-3.5 w-3.5 shrink-0 text-blue-600 dark:text-blue-400" />
              )}
              {isCloudMount && (
                <Cloud className="h-3.5 w-3.5 shrink-0 text-blue-600 dark:text-blue-400" />
              )}
            </>
          ) : (
            <File className="h-3.5 w-3.5 shrink-0 text-muted-foreground/60" />
          )}
          <span className="flex-1 truncate">{node.name}</span>
          {!node.is_dir && node.size > 0 && (
            <span className="shrink-0 text-[10px] text-muted-foreground/40">
              {formatSize(node.size)}
            </span>
          )}
        </button>
        {node.expanded &&
          node.children?.map((child) => renderNode(child, depth + 1))}
      </div>
    );
  }

  return (
    <>
      <div className="flex h-full flex-col">
        <div className="flex items-center justify-between border-b border-border px-3 py-2">
          <span className="text-xs font-medium text-muted-foreground">
            Files
          </span>
          <button
            onClick={loadMounts}
            className="text-muted-foreground/50 transition-colors hover:text-muted-foreground"
            title="Refresh mounts"
          >
            <RefreshCw className="h-3 w-3" />
          </button>
        </div>

        <div className="flex-1 overflow-y-auto scroll-container py-1">
          {configLoading ? (
            <div className="flex items-center justify-center py-8 text-muted-foreground/40">
              <Loader2 className="h-4 w-4 animate-spin" />
            </div>
          ) : configError ? (
            <p className="p-3 text-xs text-red-600 dark:text-red-400">
              {configError}
            </p>
          ) : roots.length === 0 ? (
            <p className="p-3 text-xs text-muted-foreground/50">
              No mounts configured.
            </p>
          ) : (
            roots.map((root) => renderNode(root, 0))
          )}
        </div>
      </div>

      <Dialog
        open={preview !== null}
        onOpenChange={(open) => {
          if (!open) setPreview(null);
        }}
      >
        <DialogContent className={CODE_DIALOG_CLASS}>
          <div className="pr-6">
            <DialogTitle className="truncate font-mono text-sm font-normal text-muted-foreground">
              {preview?.path}
            </DialogTitle>
          </div>
          <div className="h-[55vh] overflow-hidden rounded-md border border-border">
            {preview && (
              <CodeViewer content={preview.content} lang={preview.lang} />
            )}
          </div>
        </DialogContent>
      </Dialog>
    </>
  );
}
