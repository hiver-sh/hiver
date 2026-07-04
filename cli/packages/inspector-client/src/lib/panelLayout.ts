import { getLeaves, type MosaicNode } from "react-mosaic-component";
import type { PanelId } from "@/lib/userPreferences";

// The tiling tree Mosaic renders. `null` means no panels are shown.
export type Tree = MosaicNode<PanelId> | null;

type SplitNode = Extract<MosaicNode<PanelId>, { type: "split" }>;

// Drop `id` from the tree, collapsing any container left with a single child.
function removeLeaf(node: Tree, id: PanelId): Tree {
  if (node == null) return null;
  if (typeof node === "string") return node === id ? null : node;
  if (node.type === "tabs") {
    const tabs = node.tabs.filter((t) => t !== id);
    if (tabs.length === 0) return null;
    if (tabs.length === 1) return tabs[0];
    return {
      ...node,
      tabs,
      activeTabIndex: Math.min(node.activeTabIndex, tabs.length - 1),
    };
  }
  const children = node.children
    .map((c) => removeLeaf(c, id))
    .filter((c): c is MosaicNode<PanelId> => c != null);
  if (children.length === 0) return null;
  if (children.length === 1) return children[0];
  // Only the split that actually lost a direct child has a now-mismatched size
  // array and must rebalance. Ancestors whose children are all still present
  // (a descendant collapsed, but this level's slots are unchanged) keep their
  // splitPercentages, so unrelated panels don't resize when a panel is removed
  // — e.g. the browser being hidden until CDP is detected must not reshuffle
  // the timeline/terminal proportions.
  const lostDirectChild = children.length !== node.children.length;
  return {
    type: "split",
    direction: node.direction,
    children,
    ...(lostDirectChild ? {} : { splitPercentages: node.splitPercentages }),
  };
}

// Add `id` as a new right-hand column, extending an existing top-level row
// rather than nesting another split inside it.
function addLeaf(node: Tree, id: PanelId): Tree {
  if (node == null) return id;
  if (
    typeof node !== "string" &&
    node.type === "split" &&
    node.direction === "row"
  ) {
    return { type: "split", direction: "row", children: [...node.children, id] };
  }
  return { type: "split", direction: "row", children: [node, id] };
}

// Find the split that has `id` as a direct child leaf, anywhere in the tree.
function findParentSplit(node: Tree, id: PanelId): SplitNode | null {
  if (node == null || typeof node === "string" || node.type === "tabs") {
    return null;
  }
  if (node.children.some((c) => c === id)) return node as SplitNode;
  for (const c of node.children) {
    const found = findParentSplit(c, id);
    if (found) return found;
  }
  return null;
}

function leafSet(node: MosaicNode<PanelId>): Set<PanelId> {
  return new Set(getLeaves(node));
}
function sameSet(a: Set<PanelId>, b: Set<PanelId>): boolean {
  return a.size === b.size && [...a].every((x) => b.has(x));
}

// Replace the outermost subtree whose leaves exactly equal `target` with
// `build(subtree)`; returns `node` unchanged if nothing matches.
function replaceByLeaves(
  node: MosaicNode<PanelId>,
  target: Set<PanelId>,
  build: (sub: MosaicNode<PanelId>) => MosaicNode<PanelId>,
): MosaicNode<PanelId> {
  if (sameSet(leafSet(node), target)) return build(node);
  if (typeof node === "string" || node.type === "tabs") return node;
  return {
    ...node,
    children: node.children.map((c) => replaceByLeaves(c, target, build)),
  };
}

// When a layout is edited (resized/rearranged) while some panels are hidden —
// e.g. the browser before CDP is detected — those panels are absent from the
// rendered tree, so persisting it verbatim would lose their placement (they'd
// later re-appear appended as a new column via addLeaf). Graft each hidden
// panel back beside the sibling it sits next to in the saved layout, keeping
// the sibling's freshly-edited sizes so the visible panels don't move.
export function reinsertHidden(
  rendered: Tree,
  saved: Tree,
  hidden: PanelId[],
): Tree {
  if (rendered == null) return rendered;
  let tree: MosaicNode<PanelId> = rendered;
  for (const id of hidden) {
    const parent = findParentSplit(saved, id);
    if (!parent) continue;
    const idx = parent.children.findIndex((c) => c === id);
    if (idx === -1) continue;
    const siblingLeaves = new Set(
      parent.children.filter((_, i) => i !== idx).flatMap((s) => getLeaves(s)),
    );
    if (siblingLeaves.size === 0) continue;
    tree = replaceByLeaves(tree, siblingLeaves, (sub) => {
      // Two-child split: re-pair the panel with the sibling's edited subtree at
      // its original side and ratio.
      if (parent.children.length === 2) {
        return {
          type: "split",
          direction: parent.direction,
          children: idx === 0 ? [id, sub] : [sub, id],
          ...(parent.splitPercentages
            ? { splitPercentages: parent.splitPercentages }
            : {}),
        };
      }
      // Wider split: those siblings were rebalanced on removal anyway, so
      // restore the saved split wholesale (it already holds the panel in place).
      return parent;
    });
  }
  return tree;
}

// Reconcile a stored tree against the set of currently-visible panels: drop any
// panel that's now hidden, and append any newly-visible one. `visible` is in the
// order new panels should be added.
export function reconcileTree(stored: Tree, visible: PanelId[]): Tree {
  // Self-heal corrupt/legacy persisted layouts: getLeaves surfaces a malformed
  // sub-node (e.g. a stray v6 `{ direction, first, second }`) as an opaque
  // object leaf, and duplicate leaves crash Mosaic outright. In either case the
  // stored tree is unusable, so discard it and rebuild fresh from `visible`.
  const leaves = stored ? getLeaves(stored) : [];
  const usable =
    (leaves as unknown[]).every((l) => typeof l === "string") &&
    new Set(leaves).size === leaves.length;
  let tree: Tree = usable ? stored : null;
  for (const leaf of tree ? getLeaves(tree) : []) {
    if (!visible.includes(leaf)) tree = removeLeaf(tree, leaf);
  }
  const present = new Set(tree ? getLeaves(tree) : []);
  for (const id of visible) {
    if (!present.has(id)) tree = addLeaf(tree, id);
  }
  return tree;
}
