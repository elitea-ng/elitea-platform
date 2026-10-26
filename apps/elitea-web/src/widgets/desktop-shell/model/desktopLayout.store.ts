/**
 * The desktop shell's window layout: which panes are open, how wide the
 * changes panel is, the command palette, the window chrome the host drew,
 * and the actions of the workspace session on screen (so a shortcut, the
 * palette or the host's menu can reach "new thread" / "plan mode").
 *
 * Purely local UI state, so a zustand store (R-S1); built by a factory and
 * memoised on first use (R-S2, same shape as `sidebarCollapsed.store.ts`).
 * Pane visibility and the inspector's layout (per kind of page: its groups,
 * tabs, split and width) persist per viewer (`desktopLayoutPersistence.ts`).
 */
import { create, type StoreApi, type UseBoundStore } from 'zustand';

import { BROWSER_PLATFORM, type AppPlatform } from '@/shared/desktop/appEvents';
import { INSPECTOR_TAB_PAGE, type InspectorPage, type InspectorTabId } from '@/shared/desktop/inspectorSlot';

import {
  activateTab,
  clampInspectorWidth,
  clampSplit,
  closeTab,
  defaultInspectorLayout,
  moveTab,
  reopenIfAllClosed,
  splitTab,
  viewOf,
  type InspectorLayout,
} from '../lib/inspectorLayout';

/** What the session on screen offers to the shell's commands. */
export interface SessionActions {
  workspaceId: string;
  newThread: () => void;
  togglePlanMode: () => void;
  /** Thread / Folder menu entries; absent when the thread cannot do it now. */
  sendToCloud?: () => void;
  openInChat?: () => void;
  chooseAgent?: () => void;
  indexSettings?: () => void;
  handoffSettings?: () => void;
  /** Files dropped on the window: the composer attaches the images among them (#1229); absent when it takes none. */
  attachFiles?: ((paths: string[]) => void) | undefined;
}

/** What the page on screen offers the inspector: its kind and its tabs (none: `page` is `null`). */
export interface InspectorOffer {
  readonly page: InspectorPage | null;
  readonly tabs: readonly InspectorTabId[];
}

export interface DesktopLayoutState {
  readonly sidebarOpen: boolean;
  /** The inspector (the right panel) is open: one toggle, ⌘⌥\\, for every page. */
  readonly changesOpen: boolean;
  /** The inspector's layout per kind of page. */
  readonly inspector: Readonly<Record<InspectorPage, InspectorLayout>>;
  /** The tabs the page on screen offers (the frame's `useInspectorHost` reports them). Not remembered. */
  readonly inspectorOffer: InspectorOffer;
  /**
   * The inspector temporarily takes the main column's place (Esc restores).
   * Not remembered; dropped whenever the panel stops being on screen (hidden,
   * its last tab closed, the page gone) or the kind of page changes.
   */
  readonly inspectorMaximized: boolean;
  /** The tabs open in a window of their own (the frame's panel windows). Not remembered. */
  readonly poppedTabs: readonly InspectorTabId[];
  readonly paletteOpen: boolean;
  readonly platform: AppPlatform;
  /**
   * The host answered `app_platform`: its menu bar owns the shortcuts (⌘K,
   * ⌘N, …) and sends them as `app://command`, so the page must not bind them
   * too (each would fire twice).
   */
  readonly hostMenu: boolean;
  /** A message for the person (e.g. the host could not open a folder). */
  readonly notice: string | null;
  readonly session: SessionActions | null;
  readonly setSidebarOpen: (open: boolean) => void;
  /** Shows or hides the inspector; showing it when every one of the page's tabs is closed reopens them. */
  readonly setChangesOpen: (open: boolean) => void;
  /** ⌘⌥\\ and the toggles: hides the panel while it is on screen, otherwise shows it (reopening the tabs if all are closed). */
  readonly toggleInspector: () => void;
  readonly setInspectorOffer: (offer: InspectorOffer) => void;
  /** Open the inspector on `tab`, reopening it if it was closed (the Folder menu's Index settings…, an approval to review). */
  readonly showInspector: (tab: InspectorTabId) => void;
  /** Make `tab` its group's active tab (a click on its chip). */
  readonly activateInspectorTab: (tab: InspectorTabId) => void;
  /** Hide `tab` (× on its chip) until it is reopened from the ⋯ menu. */
  readonly closeInspectorTab: (tab: InspectorTabId) => void;
  /** Move `tab` to the top (0) or bottom (1) group. */
  readonly moveInspectorTab: (tab: InspectorTabId, to: 0 | 1) => void;
  /** Split `tab` off into a group below `keep` (the one group on screen). */
  readonly splitInspectorTab: (tab: InspectorTabId, keep: number) => void;
  readonly setInspectorSplit: (page: InspectorPage, split: number) => void;
  readonly setInspectorWidth: (page: InspectorPage, width: number) => void;
  readonly setInspectorMaximized: (maximized: boolean) => void;
  readonly setTabPopped: (tab: InspectorTabId, popped: boolean) => void;
  readonly setPaletteOpen: (open: boolean) => void;
  readonly setPlatform: (platform: AppPlatform, hostMenu: boolean) => void;
  readonly setNotice: (notice: string | null) => void;
  readonly setSession: (session: SessionActions | null) => void;
  /** Adds the on-screen thread's optional actions to the registered session (no-op without one). */
  readonly extendSession: (actions: Partial<SessionActions>) => void;
}

type DesktopLayoutStore = UseBoundStore<StoreApi<DesktopLayoutState>>;

export const DEFAULT_INSPECTOR: Readonly<Record<InspectorPage, InspectorLayout>> = {
  local: defaultInspectorLayout('local'),
  chat: defaultInspectorLayout('chat'),
};

/** The inspector is on screen: open, on a page with tabs, at least one of them not closed. */
export function selectInspectorShown(state: Pick<DesktopLayoutState, 'changesOpen' | 'inspector' | 'inspectorOffer'>): boolean {
  const { page, tabs } = state.inspectorOffer;
  return state.changesOpen && page !== null && viewOf(state.inspector[page], tabs).length > 0;
}

export function createDesktopLayoutStore(): DesktopLayoutStore {
  return create<DesktopLayoutState>((rawSet) => {
    // Every change goes through here: a maximized panel that stops being on screen, or whose page changed, is restored.
    const set = (change: Partial<DesktopLayoutState> | ((state: DesktopLayoutState) => Partial<DesktopLayoutState>)): void =>
      rawSet((state) => {
        const patch = typeof change === 'function' ? change(state) : change;
        const next = { ...state, ...patch };
        if (patch === state || !next.inspectorMaximized) return patch;
        const pageChanged = next.inspectorOffer.page !== state.inspectorOffer.page;
        return pageChanged || !selectInspectorShown(next) ? { ...patch, inspectorMaximized: false } : patch;
      });
    const layoutOf = (page: InspectorPage, change: (layout: InspectorLayout) => InspectorLayout): void =>
      set((state) => ({ inspector: { ...state.inspector, [page]: change(state.inspector[page]) } }));
    const open = (state: DesktopLayoutState): Partial<DesktopLayoutState> => {
      const { page, tabs } = state.inspectorOffer;
      return page === null ? { changesOpen: true } : { changesOpen: true, inspector: { ...state.inspector, [page]: reopenIfAllClosed(state.inspector[page], tabs) } };
    };
    /** Applies `change` to the layout of the page `tab` belongs to. */
    const onPage = (tab: InspectorTabId, change: (layout: InspectorLayout) => InspectorLayout): void => layoutOf(INSPECTOR_TAB_PAGE[tab], change);
    return {
      sidebarOpen: true,
      changesOpen: true,
      inspector: DEFAULT_INSPECTOR,
      inspectorOffer: { page: null, tabs: [] },
      inspectorMaximized: false,
      poppedTabs: [],
      paletteOpen: false,
      platform: BROWSER_PLATFORM,
      hostMenu: false,
      notice: null,
      session: null,
      setSidebarOpen: (sidebarOpen) => set({ sidebarOpen }),
      setChangesOpen: (changesOpen) => set((state) => (changesOpen ? open(state) : { changesOpen: false })),
      // On a page without tabs it is the plain remembered toggle; with tabs it toggles what is on screen.
      toggleInspector: () =>
        set((state) => {
          const hide = state.inspectorOffer.page === null ? state.changesOpen : selectInspectorShown(state);
          return hide ? { changesOpen: false } : open(state);
        }),
      setInspectorOffer: (offer) =>
        set((state) => {
          const current = state.inspectorOffer;
          const same = current.page === offer.page && current.tabs.length === offer.tabs.length && current.tabs.every((tab, index) => offer.tabs[index] === tab);
          return same ? state : { inspectorOffer: offer };
        }),
      showInspector: (tab) => {
        onPage(tab, (layout) => activateTab(layout, tab));
        set({ changesOpen: true });
      },
      activateInspectorTab: (tab) => onPage(tab, (layout) => activateTab(layout, tab)),
      closeInspectorTab: (tab) => onPage(tab, (layout) => closeTab(layout, tab)),
      moveInspectorTab: (tab, to) => onPage(tab, (layout) => moveTab(layout, tab, to)),
      splitInspectorTab: (tab, keep) => onPage(tab, (layout) => splitTab(layout, tab, keep)),
      setInspectorSplit: (page, split) => layoutOf(page, (layout) => ({ ...layout, split: clampSplit(split) })),
      setInspectorWidth: (page, width) => layoutOf(page, (layout) => ({ ...layout, width: clampInspectorWidth(width) })),
      setInspectorMaximized: (inspectorMaximized) => set({ inspectorMaximized }),
      setTabPopped: (tab, popped) =>
        set((state) => ({ poppedTabs: popped ? (state.poppedTabs.includes(tab) ? state.poppedTabs : [...state.poppedTabs, tab]) : state.poppedTabs.filter((id) => id !== tab) })),
      setPaletteOpen: (paletteOpen) => set({ paletteOpen }),
      setPlatform: (platform, hostMenu) => set({ platform, hostMenu }),
      setNotice: (notice) => set({ notice }),
      setSession: (session) => set({ session }),
      extendSession: (actions) => set((state) => (state.session === null ? {} : { session: { ...state.session, ...actions } })),
    };
  });
}

let instance: DesktopLayoutStore | undefined;

function resolveStore(): DesktopLayoutStore {
  instance ??= createDesktopLayoutStore();
  return instance;
}

function useDesktopLayoutHook<T>(selector: (state: DesktopLayoutState) => T): T {
  return resolveStore()(selector);
}

/** The lazily-constructed singleton, with the zustand `getState`/`setState` surface. */
export const useDesktopLayout = Object.assign(useDesktopLayoutHook, {
  getState: (): DesktopLayoutState => resolveStore().getState(),
  setState: (partial: Partial<DesktopLayoutState>): void => resolveStore().setState(partial),
  subscribe: (listener: (state: DesktopLayoutState) => void): (() => void) => resolveStore().subscribe(listener),
});
