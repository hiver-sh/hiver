/**
 * Local-folder access via a `<input type="file" webkitdirectory>` element.
 * The user picks a folder once; the browser hands us every file in it (with a
 * `webkitRelativePath`), which we flatten into a list the composer's `@` menu
 * can reference. Unlike `showDirectoryPicker`, this has no system-folder
 * block-list, so it works for any project directory and across browsers.
 */

export type FolderEntry = {
  /** Folder-relative path, e.g. `src/index.ts` — what `@` inserts. */
  relPath: string;
  /** Basename, uploaded as `/workspace/<name>`. */
  name: string;
  getFile: () => Promise<File>;
};

export type FolderState = {
  name: string;
  entries: FolderEntry[];
};

const IGNORED = new Set(["node_modules", ".git", ".next", "dist", "out"]);
const MAX_ENTRIES = 4000;

export function supportsFolderPicker(): boolean {
  if (typeof document === "undefined") return false;
  return "webkitdirectory" in document.createElement("input");
}

/** Build a folder from the `FileList` a `webkitdirectory` input yields. */
export function buildFolder(fileList: FileList | File[]): FolderState | null {
  const files = Array.from(fileList);
  if (files.length === 0) return null;

  const folderName =
    files[0].webkitRelativePath.split("/")[0] || files[0].name || "folder";

  const entries: FolderEntry[] = [];
  for (const file of files) {
    const rel = file.webkitRelativePath || file.name;
    const parts = rel.split("/");
    // Skip build/vendor/dotfile paths so the `@` menu stays useful.
    if (parts.some((p) => IGNORED.has(p) || p.startsWith("."))) continue;
    // Drop the leading folder-name segment so references read cleanly.
    const relPath = parts.length > 1 ? parts.slice(1).join("/") : rel;
    entries.push({ relPath, name: file.name, getFile: async () => file });
    if (entries.length >= MAX_ENTRIES) break;
  }

  entries.sort((a, b) => a.relPath.localeCompare(b.relPath));
  return { name: folderName, entries };
}
