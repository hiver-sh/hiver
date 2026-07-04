import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
} from "react";
import type { ReactNode } from "react";
import type { MosaicNode } from "react-mosaic-component";

export type Theme = "light" | "dark" | "system";

/** Identifies a resizable panel in the sandbox detail layout. */
export type PanelId = "timeline" | "terminal" | "browser" | "files";

/** Every panel, in the canonical default order. */
export const ALL_PANELS: PanelId[] = [
  "timeline",
  "terminal",
  "browser",
  "files",
];

export interface UserPreferences {
  theme: Theme;
  sidebarCollapsed: boolean;
  showTerminal: boolean;
  /** Whether the browser (CDP screencast) panel is shown. Only takes effect for
   *  sandboxes that actually expose a CDP endpoint. */
  showBrowser: boolean;
  showFiles: boolean;
  showTimeline: boolean;
  /** The tiling layout of the panels — a react-mosaic tree of row/column splits.
   *  Panels can sit side-by-side or stacked in a column, in any nesting. Only
   *  visible panels appear; the tree is reconciled against visibility on load.
   *  null = derive a default arrangement from the visible panels. */
  panelLayout: MosaicNode<PanelId> | null;
  followEvents: boolean;
  /** Expanded file-explorer directories, keyed by sandbox key. Each sandbox
   *  restores only its own expanded folders; an entry is dropped when the
   *  sandbox is destroyed (see `forgetSandbox`). */
  expandedPaths: Record<string, string[]>;
  /** When false, the terminal never writes selections to the system clipboard,
   *  avoiding the browser's clipboard-permission prompt. */
  terminalClipboardCopy: boolean;
}

export const DEFAULT_PREFS: UserPreferences = {
  theme: "system",
  sidebarCollapsed: false,
  showTerminal: true,
  showBrowser: true,
  showFiles: true,
  showTimeline: true,
  panelLayout: null,
  followEvents: false,
  expandedPaths: {},
  terminalClipboardCopy: true,
};

const STORAGE_KEY = "inspector:prefs";

function loadFromStorage(): Partial<UserPreferences> {
  try {
    const parsed = JSON.parse(
      localStorage.getItem(STORAGE_KEY) ?? "{}",
    ) as Partial<UserPreferences>;
    // Migrate the legacy global `expandedPaths: string[]` to the per-sandbox
    // map shape. Old data isn't worth keying to any sandbox, so drop it.
    if (Array.isArray(parsed.expandedPaths)) parsed.expandedPaths = {};
    return parsed;
  } catch {
    return {};
  }
}

function saveToStorage(prefs: UserPreferences) {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(prefs));
}

function applyTheme(theme: Theme) {
  const root = document.documentElement;
  if (theme === "system") {
    root.classList.toggle(
      "dark",
      window.matchMedia("(prefers-color-scheme: dark)").matches,
    );
  } else {
    root.classList.toggle("dark", theme === "dark");
  }
}

export interface UserPreferencesContextValue {
  prefs: UserPreferences;
  setPref<K extends keyof UserPreferences>(
    key: K,
    value: UserPreferences[K],
  ): void;
  toggleExpandedPath(sandboxKey: string, path: string): void;
  /** Drop a sandbox's persisted file-explorer state when it is destroyed. */
  forgetSandbox(sandboxKey: string): void;
  terminalScrollPassthrough: boolean;
  enableNetworkRequests: boolean;
  showHeader: boolean;
}

const UserPreferencesContext = createContext<UserPreferencesContextValue>({
  prefs: DEFAULT_PREFS,
  setPref: () => {},
  toggleExpandedPath: () => {},
  forgetSandbox: () => {},
  terminalScrollPassthrough: false,
  enableNetworkRequests: true,
  showHeader: true,
});

export function useUserPreferences(): UserPreferencesContextValue {
  return useContext(UserPreferencesContext);
}

export function UserPreferencesProvider({
  children,
  initialPreferences,
  persist = true,
  terminalScrollPassthrough = false,
  enableNetworkRequests = true,
  showHeader = true,
}: {
  children: ReactNode;
  initialPreferences?: Partial<UserPreferences>;
  /** When false, preferences are not read from or written to localStorage —
   *  they live only for the lifetime of this provider. Defaults to true. */
  persist?: boolean;
  /** When true, wheel events inside the terminal bubble up to the page instead
   *  of being consumed by xterm's scrollback. */
  terminalScrollPassthrough?: boolean;
  /** When false, components should suppress live network requests (e.g. API
   *  polling, SSE connections). Defaults to true. */
  enableNetworkRequests?: boolean;
  /** When false, the sandbox detail header is hidden. Defaults to true. */
  showHeader?: boolean;
}) {
  const [prefs, setPrefs] = useState<UserPreferences>(() => {
    const merged = {
      ...DEFAULT_PREFS,
      ...(persist ? loadFromStorage() : {}),
      ...initialPreferences,
    };
    return merged;
  });

  const setPref = useCallback(
    <K extends keyof UserPreferences>(key: K, value: UserPreferences[K]) => {
      setPrefs((prev) => {
        const next = { ...prev, [key]: value };
        if (persist) saveToStorage(next);
        return next;
      });
    },
    [persist],
  );

  const toggleExpandedPath = useCallback(
    (sandboxKey: string, path: string) => {
      setPrefs((prev) => {
        const current = prev.expandedPaths[sandboxKey] ?? [];
        const paths = current.includes(path)
          ? current.filter((p) => p !== path)
          : [...current, path];
        const nextMap = { ...prev.expandedPaths };
        if (paths.length === 0) delete nextMap[sandboxKey];
        else nextMap[sandboxKey] = paths;
        const next = { ...prev, expandedPaths: nextMap };
        if (persist) saveToStorage(next);
        return next;
      });
    },
    [persist],
  );

  const forgetSandbox = useCallback(
    (sandboxKey: string) => {
      setPrefs((prev) => {
        if (!(sandboxKey in prev.expandedPaths)) return prev;
        const nextMap = { ...prev.expandedPaths };
        delete nextMap[sandboxKey];
        const next = { ...prev, expandedPaths: nextMap };
        if (persist) saveToStorage(next);
        return next;
      });
    },
    [persist],
  );

  // Apply theme to DOM
  useEffect(() => {
    applyTheme(prefs.theme);
  }, [prefs.theme]);

  // Keep system theme in sync with OS preference
  useEffect(() => {
    if (prefs.theme !== "system") return;
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const handler = () => applyTheme("system");
    mq.addEventListener("change", handler);
    return () => mq.removeEventListener("change", handler);
  }, [prefs.theme]);

  return (
    <UserPreferencesContext.Provider
      value={{
        prefs,
        setPref,
        toggleExpandedPath,
        forgetSandbox,
        terminalScrollPassthrough,
        enableNetworkRequests,
        showHeader,
      }}
    >
      {children}
    </UserPreferencesContext.Provider>
  );
}
