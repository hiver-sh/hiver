import { useEffect, useState } from "react";
import { useUserPreferences } from "@/lib/userPreferences";

export const MONACO_DARK_THEME = "hive-dark";
export const MONACO_LIGHT_THEME = "hive-light";
export const MONACO_DARK_MUTED_THEME = "hive-dark-muted";
export const MONACO_LIGHT_MUTED_THEME = "hive-light-muted";

export function useMonacoTheme(muted = false): string {
  const { prefs } = useUserPreferences();
  const [systemDark, setSystemDark] = useState(
    () => window.matchMedia("(prefers-color-scheme: dark)").matches,
  );

  useEffect(() => {
    if (prefs.theme !== "system") return;
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const handler = () => setSystemDark(mq.matches);
    mq.addEventListener("change", handler);
    return () => mq.removeEventListener("change", handler);
  }, [prefs.theme]);

  const isDark =
    prefs.theme === "dark" || (prefs.theme === "system" && systemDark);
  if (muted) return isDark ? MONACO_DARK_MUTED_THEME : MONACO_LIGHT_MUTED_THEME;
  return isDark ? MONACO_DARK_THEME : MONACO_LIGHT_THEME;
}
