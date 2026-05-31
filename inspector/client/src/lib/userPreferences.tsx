import { createContext, useCallback, useContext, useEffect, useState } from "react";
import type { ReactNode } from "react";

export type Theme = "light" | "dark" | "system";

export interface UserPreferences {
  theme: Theme;
  sidebarCollapsed: boolean;
  showTerminal: boolean;
  showFiles: boolean;
  showTimeline: boolean;
  terminalWidth: number;
  filesWidth: number;
  followEvents: boolean;
  expandedPaths: string[];
  /** When false, the terminal never writes selections to the system clipboard,
   *  avoiding the browser's clipboard-permission prompt. */
  terminalClipboardCopy: boolean;
}

export const DEFAULT_PREFS: UserPreferences = {
  theme: "system",
  sidebarCollapsed: false,
  showTerminal: false,
  showFiles: false,
  showTimeline: true,
  terminalWidth: 480,
  filesWidth: 256,
  followEvents: false,
  expandedPaths: [],
  terminalClipboardCopy: true,
};

const STORAGE_KEY = "inspector:prefs";

function loadFromStorage(): Partial<UserPreferences> {
  try {
    return JSON.parse(localStorage.getItem(STORAGE_KEY) ?? "{}") as Partial<UserPreferences>;
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
    root.classList.toggle("dark", window.matchMedia("(prefers-color-scheme: dark)").matches);
  } else {
    root.classList.toggle("dark", theme === "dark");
  }
}

export interface UserPreferencesContextValue {
  prefs: UserPreferences;
  setPref<K extends keyof UserPreferences>(key: K, value: UserPreferences[K]): void;
  toggleExpandedPath(path: string): void;
  terminalScrollPassthrough: boolean;
  enableNetworkRequests: boolean;
}

const UserPreferencesContext = createContext<UserPreferencesContextValue>({
  prefs: DEFAULT_PREFS,
  setPref: () => {},
  toggleExpandedPath: () => {},
  terminalScrollPassthrough: false,
  enableNetworkRequests: true,
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
}) {
  const [prefs, setPrefs] = useState<UserPreferences>(() => ({
    ...DEFAULT_PREFS,
    ...(persist ? loadFromStorage() : {}),
    ...initialPreferences,
  }));

  const setPref = useCallback(<K extends keyof UserPreferences>(key: K, value: UserPreferences[K]) => {
    setPrefs((prev) => {
      const next = { ...prev, [key]: value };
      if (persist) saveToStorage(next);
      return next;
    });
  }, [persist]);

  const toggleExpandedPath = useCallback((path: string) => {
    setPrefs((prev) => {
      const paths = prev.expandedPaths.includes(path)
        ? prev.expandedPaths.filter((p) => p !== path)
        : [...prev.expandedPaths, path];
      const next = { ...prev, expandedPaths: paths };
      if (persist) saveToStorage(next);
      return next;
    });
  }, [persist]);

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
    <UserPreferencesContext.Provider value={{ prefs, setPref, toggleExpandedPath, terminalScrollPassthrough, enableNetworkRequests }}>
      {children}
    </UserPreferencesContext.Provider>
  );
}
