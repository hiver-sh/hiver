import { useCallback, useEffect, useMemo, useState } from "react";
import { convertLegacyToNary, getLeaves } from "react-mosaic-component";
import { useUserPreferences } from "@/lib/userPreferences";
import type { PanelId } from "@/lib/userPreferences";
import { reconcileTree, reinsertHidden, type Tree } from "@/lib/panelLayout";

/**
 * Owns the tiling tree Mosaic renders. The tree is kept in local state so live
 * drags/resizes update smoothly, and persisted to prefs only when a change
 * completes (`persistLayout`, wired to Mosaic.onRelease).
 *
 * We reconcile against the *saved* layout, not the previous rendered one, so a
 * panel's position is retained even while it's temporarily hidden (e.g. the
 * browser before CDP is detected) rather than popping out to a new column.
 *
 * `visiblePanels` is the set of currently-visible panels, in canonical order
 * (used when appending newly-visible tiles).
 */
export function usePanelLayout(visiblePanels: PanelId[]) {
  const { prefs, setPref } = useUserPreferences();

  // Normalize any persisted layout to the n-ary (v7) shape — older sessions may
  // have stored the legacy `{ direction, first, second }` form. Memoized so the
  // reconcile effect below doesn't re-run on every render from a fresh object.
  const savedLayout = useMemo(
    () => convertLegacyToNary(prefs.panelLayout),
    [prefs.panelLayout],
  );

  const [layout, setLayout] = useState<Tree>(() =>
    reconcileTree(savedLayout, visiblePanels),
  );

  const visibleKey = visiblePanels.join(",");
  useEffect(() => {
    setLayout(reconcileTree(savedLayout, visiblePanels));
  }, [savedLayout, visibleKey]); // eslint-disable-line react-hooks/exhaustive-deps

  // Persist a completed layout edit, re-grafting any panel that's in the saved
  // layout but hidden right now (absent from `next`) so editing the layout while
  // it's hidden — the browser before CDP is detected — doesn't discard its
  // placement.
  const persistLayout = useCallback(
    (next: Tree) => {
      const renderedLeaves = new Set(next ? getLeaves(next) : []);
      const hidden = (savedLayout ? getLeaves(savedLayout) : []).filter(
        (id) => !renderedLeaves.has(id),
      );
      setPref(
        "panelLayout",
        hidden.length ? reinsertHidden(next, savedLayout, hidden) : next,
      );
    },
    [savedLayout, setPref],
  );

  return { layout, setLayout, persistLayout };
}
