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
}

export interface DesktopLayoutState {
  readonly sidebarOpen: boolean;
  /** The inspector (the right panel) is open: one toggle, ⌘⌥\\, for every page. */
  readonly changesOpen: boolean;
  /** The inspector's layout per kind of page. */
  readonly inspector: Readonly<Record<InspectorPage, InspectorLayout>>;
  /** The inspector temporarily takes the main column's place (Esc restores). Not remembered. */
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
  readonly setChangesOpen: (open: boolean) => void;
  /** Open the inspector on `tab`, reopening it if it was closed (the Folder menu's Index settings…, an approval to review). */
  readonly showInspector: (tab: InspectorTabId) => void;
  /** Make `tab` its group's active tab (a click on its chip). */
  readonly activateInspectorTab: (tab: InspectorTabId) => void;
  /** Hide `tab` (× on its chip) until it is reopened from the ⋯ menu. */
  readonly closeInspectorTab: (tab: InspectorTabId) => void;
  /** Move `tab` to the top (0) or bottom (1) group. */
  readonly moveInspectorTab: (tab: InspectorTabId, to: 0 | 1) => void;
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

export function createDesktopLayoutStore(): DesktopLayoutStore {
  return create<DesktopLayoutState>((set) => {
    const layoutOf = (page: InspectorPage, change: (layout: InspectorLayout) => InspectorLayout): void =>
      set((state) => ({ inspector: { ...state.inspector, [page]: change(state.inspector[page]) } }));
    /** Applies `change` to the layout of the page `tab` belongs to. */
    const onPage = (tab: InspectorTabId, change: (layout: InspectorLayout) => InspectorLayout): void => layoutOf(INSPECTOR_TAB_PAGE[tab], change);
    return {
      sidebarOpen: true,
      changesOpen: true,
      inspector: DEFAULT_INSPECTOR,
      inspectorMaximized: false,
      poppedTabs: [],
      paletteOpen: false,
      platform: BROWSER_PLATFORM,
      hostMenu: false,
      notice: null,
      session: null,
      setSidebarOpen: (sidebarOpen) => set({ sidebarOpen }),
      setChangesOpen: (changesOpen) => set({ changesOpen }),
      showInspector: (tab) => {
        onPage(tab, (layout) => activateTab(layout, tab));
        set({ changesOpen: true });
      },
      activateInspectorTab: (tab) => onPage(tab, (layout) => activateTab(layout, tab)),
      closeInspectorTab: (tab) => onPage(tab, (layout) => closeTab(layout, tab)),
      moveInspectorTab: (tab, to) => onPage(tab, (layout) => moveTab(layout, tab, to)),
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
