import { ChevronDown, ChevronRight, File, Folder, FolderOpen, Loader2, RefreshCw } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { cn } from "@/lib/utils";
import { CodeViewer } from "@/components/CodeViewer";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";

// Extensions that open in the Monaco viewer; all others trigger a download.
const TEXT_LANGS: Record<string, string> = {
  md: "markdown", markdown: "markdown",
  txt: "plaintext", log: "plaintext", csv: "plaintext", env: "plaintext",
  json: "json", jsonc: "json",
  js: "javascript", jsx: "javascript", mjs: "javascript", cjs: "javascript",
  ts: "typescript", tsx: "typescript",
  py: "python",
  yaml: "yaml", yml: "yaml",
  html: "html", htm: "html",
  css: "css", scss: "scss", less: "less",
  sh: "shell", bash: "shell", zsh: "shell",
  toml: "ini", ini: "ini",
  xml: "xml", svg: "xml",
  sql: "sql",
  go: "go",
  rs: "rust",
  rb: "ruby",
  php: "php",
  java: "java",
  c: "c", h: "cpp", cpp: "cpp", cc: "cpp",
  cs: "csharp",
  swift: "swift",
  kt: "kotlin",
  r: "r",
  lua: "lua",
  dockerfile: "dockerfile",
};

function langForPath(path: string): string | null {
  const name = path.split("/").pop() ?? "";
  // Bare filenames like "Dockerfile"
  const bare = TEXT_LANGS[name.toLowerCase()];
  if (bare) return bare;
  const ext = name.includes(".") ? name.split(".").pop()!.toLowerCase() : "";
  return TEXT_LANGS[ext] ?? null;
}

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
  sandboxId: string;
  serverUrl: string;
  controllerUrl: string;
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

export function FileExplorer({ sandboxId, serverUrl, controllerUrl }: Props) {
  const [roots, setRoots] = useState<TreeNode[]>([]);
  const [configLoading, setConfigLoading] = useState(true);
  const [configError, setConfigError] = useState<string | null>(null);
  const [preview, setPreview] = useState<Preview | null>(null);
  const [loadingPath, setLoadingPath] = useState<string | null>(null);

  const fileUrl = useCallback(
    (path: string) => {
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(sandboxId)}/file`,
      );
      url.searchParams.set("path", path);
      url.searchParams.set("controller", controllerUrl);
      return url;
    },
    [sandboxId, serverUrl, controllerUrl],
  );

  const loadMounts = useCallback(async () => {
    setConfigLoading(true);
    setConfigError(null);
    try {
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(sandboxId)}/config`,
      );
      url.searchParams.set("controller", controllerUrl);
      const config = await fetch(url).then((r) => r.json() as Promise<{ fs?: { mount: string }[] }>);
      const mounts = (config.fs ?? []).map((f) => f.mount);
      setRoots(
        mounts.map((mount) => ({
          name: mount,
          path: mount,
          is_dir: true,
          size: 0,
          children: null,
          expanded: false,
          loading: false,
        })),
      );
    } catch (e) {
      setConfigError(String(e));
    } finally {
      setConfigLoading(false);
    }
  }, [sandboxId, serverUrl, controllerUrl]);

  useEffect(() => { loadMounts(); }, [loadMounts]);

  const fetchChildren = useCallback(
    async (path: string): Promise<TreeNode[]> => {
      const url = new URL(
        `${serverUrl}/api/sandboxes/${encodeURIComponent(sandboxId)}/directories`,
      );
      url.searchParams.set("path", path);
      url.searchParams.set("controller", controllerUrl);
      const data = await fetch(url).then((r) => r.json() as Promise<{ entries: DirEntry[] }>);
      return toNodes(data.entries);
    },
    [sandboxId, serverUrl, controllerUrl],
  );

  async function toggle(path: string) {
    const node = findNode(roots, path);
    if (!node || !node.is_dir) return;

    if (node.expanded) {
      setRoots((prev) => updateNode(prev, path, (n) => ({ ...n, expanded: false })));
      return;
    }
    if (node.children !== null) {
      setRoots((prev) => updateNode(prev, path, (n) => ({ ...n, expanded: true })));
      return;
    }
    setRoots((prev) => updateNode(prev, path, (n) => ({ ...n, loading: true })));
    try {
      const children = await fetchChildren(path);
      setRoots((prev) =>
        updateNode(prev, path, (n) => ({ ...n, loading: false, children, expanded: true })),
      );
    } catch {
      setRoots((prev) => updateNode(prev, path, (n) => ({ ...n, loading: false })));
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
      const content = await fetch(fileUrl(path)).then((r) => r.text());
      setPreview({ path, content, lang });
    } catch {
      downloadFile(path);
    } finally {
      setLoadingPath(null);
    }
  }

  function renderNode(node: TreeNode, depth: number) {
    const isLoadingThis = loadingPath === node.path;
    return (
      <div key={node.path}>
        <button
          className={cn(
            "flex w-full items-center gap-1.5 py-[3px] pr-3 text-xs text-left transition-colors hover:bg-muted/50",
            node.is_dir ? "text-foreground" : "text-muted-foreground hover:text-foreground",
          )}
          style={{ paddingLeft: 8 + depth * 14 }}
          onClick={() => (node.is_dir ? toggle(node.path) : openFile(node.path))}
          title={node.is_dir ? node.path : node.path}
          disabled={isLoadingThis}
        >
          <span className="flex h-3.5 w-3.5 shrink-0 items-center justify-center">
            {node.is_dir && (
              node.loading ? (
                <Loader2 className="h-3 w-3 animate-spin text-muted-foreground" />
              ) : node.expanded ? (
                <ChevronDown className="h-3 w-3 text-muted-foreground" />
              ) : (
                <ChevronRight className="h-3 w-3 text-muted-foreground" />
              )
            )}
            {!node.is_dir && isLoadingThis && (
              <Loader2 className="h-3 w-3 animate-spin text-muted-foreground" />
            )}
          </span>
          {node.is_dir ? (
            node.expanded ? (
              <FolderOpen className="h-3.5 w-3.5 shrink-0 text-blue-400" />
            ) : (
              <Folder className="h-3.5 w-3.5 shrink-0 text-blue-400" />
            )
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
        {node.expanded && node.children?.map((child) => renderNode(child, depth + 1))}
      </div>
    );
  }

  return (
    <>
      <div className="flex h-full flex-col">
        <div className="flex items-center justify-between border-b border-border px-3 py-2">
          <span className="text-xs font-medium text-muted-foreground">Files</span>
          <button
            onClick={loadMounts}
            className="text-muted-foreground/50 transition-colors hover:text-muted-foreground"
            title="Refresh mounts"
          >
            <RefreshCw className="h-3 w-3" />
          </button>
        </div>

        <div className="flex-1 overflow-y-auto py-1">
          {configLoading ? (
            <div className="flex items-center justify-center py-8 text-muted-foreground/40">
              <Loader2 className="h-4 w-4 animate-spin" />
            </div>
          ) : configError ? (
            <p className="p-3 text-xs text-red-400">{configError}</p>
          ) : roots.length === 0 ? (
            <p className="p-3 text-xs text-muted-foreground/50">No mounts configured.</p>
          ) : (
            roots.map((root) => renderNode(root, 0))
          )}
        </div>
      </div>

      <Dialog open={preview !== null} onOpenChange={(open) => { if (!open) setPreview(null); }}>
        <DialogContent className="max-w-4xl">
          <div className="pr-6">
            <DialogTitle className="truncate font-mono text-sm font-normal text-muted-foreground">
              {preview?.path}
            </DialogTitle>
          </div>
          <div className="h-[55vh] overflow-hidden rounded-md border border-border">
            {preview && <CodeViewer content={preview.content} lang={preview.lang} />}
          </div>
        </DialogContent>
      </Dialog>
    </>
  );
}
