import { useEffect, useState } from "react";

export const MONACO_DARK_THEME = "hive-dark";
export const MONACO_LIGHT_THEME = "hive-light";

export function useMonacoTheme(): string {
  const [isDark, setIsDark] = useState(
    () => document.documentElement.classList.contains("dark"),
  );
  useEffect(() => {
    const obs = new MutationObserver(() => {
      setIsDark(document.documentElement.classList.contains("dark"));
    });
    obs.observe(document.documentElement, { attributeFilter: ["class"] });
    return () => obs.disconnect();
  }, []);
  return isDark ? MONACO_DARK_THEME : MONACO_LIGHT_THEME;
}
