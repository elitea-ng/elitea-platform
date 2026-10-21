/**
 * The inspector's layout, as data: which tabs sit in which of its (one or
 * two, stacked) groups, each group's active tab, the tabs the person closed,
 * the split between the groups and the panel's width. One layout per kind of
 * page (`InspectorPage`): Local work and the chat each keep their own.
 *
 * The stored layout names every tab it has placed; what is on screen is the
 * stored layout narrowed to the tabs the page offers now (`viewOf`). A tab the
 * layout has not placed yet joins the top group, so a new kind of tab shows
 * up without a migration. Pure functions, so the store and the tests share
 * them.
 */
import { INSPECTOR_TAB_PAGE, type InspectorPage, type InspectorTabId } from '@/shared/desktop/inspectorSlot';

interface InspectorGroupLayout {
  readonly tabs: readonly InspectorTabId[];
  readonly active: InspectorTabId | null;
}

export interface InspectorLayout {
  /** One or two groups, top first. */
  readonly groups: readonly InspectorGroupLayout[];
  /** The tabs the person closed (× on the chip): hidden until reopened from the ⋯ menu. */
  readonly closed: readonly InspectorTabId[];
  /** The top group's share of the height while there are two (0.2–0.8). */
  readonly split: number;
  /** The panel's width, px. */
  readonly width: number;
}

export const INSPECTOR_WIDTH = { min: 260, max: 720 } as const;
export const SPLIT_RANGE = { min: 0.2, max: 0.8 } as const;

const TAB_ORDER: readonly InspectorTabId[] = ['changes', 'approvals', 'index', 'conversation'];
const TAB_IDS = new Set<string>(TAB_ORDER);

/** The tabs in their one fixed order (Changes, Approvals, Index, Conversation), however the page registered them. */
export function byTabOrder<T extends { readonly id: InspectorTabId }>(tabs: readonly T[]): T[] {
  return [...tabs].sort((a, b) => TAB_ORDER.indexOf(a.id) - TAB_ORDER.indexOf(b.id));
}

export function defaultInspectorLayout(page: InspectorPage): InspectorLayout {
  const tabs = TAB_ORDER.filter((id) => INSPECTOR_TAB_PAGE[id] === page);
  // The chat's inspector keeps the participants rail's own width (276px, on the 8px grid); Local work its changes panel's.
  return { groups: [{ tabs, active: tabs[0] ?? null }], closed: [], split: 0.5, width: page === 'chat' ? 280 : 360 };
}

export function clampInspectorWidth(width: number): number {
  if (!Number.isFinite(width)) return INSPECTOR_WIDTH.min;
  return Math.round(Math.min(INSPECTOR_WIDTH.max, Math.max(INSPECTOR_WIDTH.min, width)));
}

export function clampSplit(split: number): number {
  if (!Number.isFinite(split)) return 0.5;
  return Math.min(SPLIT_RANGE.max, Math.max(SPLIT_RANGE.min, split));
}

/** The group a tab is placed in, or -1. */
function groupOf(layout: InspectorLayout, id: InspectorTabId): number {
  return layout.groups.findIndex((group) => group.tabs.includes(id));
}

/** Drops groups that hold no tab at all (stored state, not the view). */
function compact(groups: readonly InspectorGroupLayout[]): readonly InspectorGroupLayout[] {
  const kept = groups.filter((group) => group.tabs.length > 0);
  return kept.length === 0 ? [{ tabs: [], active: null }] : kept;
}

/** Places the tab (in the top group when it has no place yet), reopens it and makes it its group's active tab. */
export function activateTab(layout: InspectorLayout, id: InspectorTabId): InspectorLayout {
  const placed = groupOf(layout, id) === -1 ? { ...layout, groups: layout.groups.map((group, index) => (index === 0 ? { ...group, tabs: [...group.tabs, id] } : group)) } : layout;
  const at = groupOf(placed, id);
  return {
    ...placed,
    closed: placed.closed.filter((tab) => tab !== id),
    groups: placed.groups.map((group, index) => (index === at ? { ...group, active: id } : group)),
  };
}

/** Hides the tab until it is reopened; its place is kept. */
export function closeTab(layout: InspectorLayout, id: InspectorTabId): InspectorLayout {
  return layout.closed.includes(id) ? layout : { ...layout, closed: [...layout.closed, id] };
}

/**
 * Moves the tab to the top (0) or bottom (1) group, where it becomes the
 * active tab. Moving to the bottom of a one-group panel splits it; the group
 * left without tabs collapses (the bottom one then becomes the only one).
 */
export function moveTab(layout: InspectorLayout, id: InspectorTabId, to: 0 | 1): InspectorLayout {
  const from = groupOf(layout, id);
  const target = Math.min(to, layout.groups.length);
  if (from === target) return activateTab(layout, id);
  const without = layout.groups.map((group) => {
    if (!group.tabs.includes(id)) return group;
    const tabs = group.tabs.filter((tab) => tab !== id);
    return { tabs, active: group.active === id ? (tabs[0] ?? null) : group.active };
  });
  const groups = target === without.length ? [...without, { tabs: [id], active: id }] : without.map((group, index) => (index === target ? { tabs: [...group.tabs, id], active: id } : group));
  return { ...layout, closed: layout.closed.filter((tab) => tab !== id), groups: compact(groups) };
}

/**
 * Splits the tab off below the group `keep` (the one group on screen): every
 * other tab gathers in the top group — `keep`'s, plus any a hidden group held —
 * and the tab alone in a bottom group, active. Unlike `moveTab`, never lands
 * the tab in a group above the one on screen.
 */
export function splitTab(layout: InspectorLayout, id: InspectorTabId, keep: number): InspectorLayout {
  const kept = layout.groups[keep];
  const order = [...(kept === undefined ? [] : [kept]), ...layout.groups.filter((_, index) => index !== keep)];
  const tabs = order.flatMap((group) => group.tabs).filter((tab) => tab !== id);
  const active = kept?.active !== undefined && kept.active !== null && kept.active !== id ? kept.active : (tabs[0] ?? null);
  return { ...layout, closed: layout.closed.filter((tab) => tab !== id), groups: compact([{ tabs, active }, { tabs: [id], active: id }]) };
}

/** Reopens the page's closed tabs when every one it offers is closed (nothing left to show otherwise). */
export function reopenIfAllClosed(layout: InspectorLayout, available: readonly InspectorTabId[]): InspectorLayout {
  if (available.length === 0 || viewOf(layout, available).length > 0) return layout;
  const offered = new Set(available);
  return { ...layout, closed: layout.closed.filter((tab) => !offered.has(tab)) };
}

export interface InspectorGroupView {
  /** The group's index in the stored layout. */
  readonly index: number;
  readonly tabs: readonly InspectorTabId[];
  readonly active: InspectorTabId;
}

/**
 * What the panel shows: the stored groups narrowed to the tabs `available`
 * now and not closed (tabs the layout never placed join the top group), the
 * empty ones dropped, each with an active tab it really has.
 */
export function viewOf(layout: InspectorLayout, available: readonly InspectorTabId[]): readonly InspectorGroupView[] {
  const offered = new Set(available);
  const placed = new Set(layout.groups.flatMap((group) => group.tabs));
  const unplaced = TAB_ORDER.filter((id) => offered.has(id) && !placed.has(id));
  const closed = new Set(layout.closed);
  const views: InspectorGroupView[] = [];
  layout.groups.forEach((group, index) => {
    const tabs = [...group.tabs, ...(index === 0 ? unplaced : [])].filter((id) => offered.has(id) && !closed.has(id));
    const first = tabs[0];
    if (first === undefined) return;
    views.push({ index, tabs, active: group.active !== null && tabs.includes(group.active) ? group.active : first });
  });
  return views;
}

/** A stored layout read back, or `null` when it is not one (corrupt persisted state is absent). */
export function parseInspectorLayout(raw: unknown, page: InspectorPage): InspectorLayout | null {
  if (typeof raw !== 'object' || raw === null) return null;
  const { groups, closed, split, width } = raw as Record<string, unknown>;
  if (!Array.isArray(groups) || !Array.isArray(closed) || typeof split !== 'number' || typeof width !== 'number') return null;
  const isTab = (value: unknown): value is InspectorTabId => typeof value === 'string' && TAB_IDS.has(value) && INSPECTOR_TAB_PAGE[value as InspectorTabId] === page;
  const seen = new Set<InspectorTabId>();
  const parsed = groups.slice(0, 2).map((group: unknown) => {
    const { tabs, active } = (group ?? {}) as Record<string, unknown>;
    const list = (Array.isArray(tabs) ? tabs : []).filter(isTab).filter((id) => {
      if (seen.has(id)) return false;
      seen.add(id);
      return true;
    });
    return { tabs: list, active: isTab(active) && list.includes(active) ? active : (list[0] ?? null) };
  });
  return { groups: compact(parsed), closed: closed.filter(isTab), split: clampSplit(split), width: clampInspectorWidth(width) };
}
