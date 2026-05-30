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
}

const UserPreferencesContext = createContext<UserPreferencesContextValue>({
  prefs: DEFAULT_PREFS,
  setPref: () => {},
});

export function useUserPreferences(): UserPreferencesContextValue {
  return useContext(UserPreferencesContext);
}

export function UserPreferencesProvider({
  children,
  initialPreferences,
}: {
  children: ReactNode;
  initialPreferences?: Partial<UserPreferences>;
}) {
  const [prefs, setPrefs] = useState<UserPreferences>(() => ({
    ...DEFAULT_PREFS,
    ...loadFromStorage(),
    ...initialPreferences,
  }));

  const setPref = useCallback(<K extends keyof UserPreferences>(key: K, value: UserPreferences[K]) => {
    setPrefs((prev) => {
      const next = { ...prev, [key]: value };
      saveToStorage(next);
      return next;
    });
  }, []);

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
    <UserPreferencesContext.Provider value={{ prefs, setPref }}>
      {children}
    </UserPreferencesContext.Provider>
  );
}
