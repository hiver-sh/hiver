import { useEffect } from "react";

export function useScrollbarVisibility() {
  useEffect(() => {
    const handleOver = (e: MouseEvent) => {
      const container = (e.target as Element).closest?.(".scroll-container");
      if (container) container.classList.add("scrollbar-visible");
    };
    const handleOut = (e: MouseEvent) => {
      const container = (e.target as Element).closest?.(".scroll-container");
      if (container && !container.contains(e.relatedTarget as Node)) {
        container.classList.remove("scrollbar-visible");
      }
    };
    document.addEventListener("mouseover", handleOver);
    document.addEventListener("mouseout", handleOut);
    return () => {
      document.removeEventListener("mouseover", handleOver);
      document.removeEventListener("mouseout", handleOut);
    };
  }, []);
}
