/** An entry of a submenu. */
export type ContextLeaf = { id: string; label: string; enabled?: boolean; run: () => void };

/**
 * One entry of a row's menu: what it says and does. A submenu holds one level
 * of entries. The hover "⋯" menu renders the list, and on the desktop the
 * same list is the native context menu (`onNativeContextMenu`).
 */
export type ContextAction = ContextLeaf | { separator: true } | { label: string; enabled?: boolean; submenu: readonly ContextLeaf[] };
