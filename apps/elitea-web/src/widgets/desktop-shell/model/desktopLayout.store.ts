/**
 * The desktop shell's window layout: which panes are open, how wide the
 * changes panel is, the command palette, the window chrome the host drew,
 * and the actions of the workspace session on screen (so a shortcut, the
 * palette or the host's menu can reach "new thread" / "plan mode").
 *
 * Purely local UI state, so a zustand store (R-S1); built by a factory and
 * memoised on first use (R-S2, same shape as `sidebarCollapsed.store.ts`).
 * Pane visibility and the panel width persist per viewer (`desktopLayoutPersistence.ts`).
 */
import { create, type StoreApi, type UseBoundStore } from 'zustand';

import { BROWSER_PLATFORM, type AppPlatform } from '@/shared/desktop/appEvents';

/** What the session on screen offers to the shell's commands. */
export interface SessionActions {
  workspaceId: string;
  newThread: () => void;
  togglePlanMode: () => void;
}

export const CHANGES_WIDTH = { min: 280, max: 720, initial: 360 } as const;

export interface DesktopLayoutState {
  readonly sidebarOpen: boolean;
  readonly changesOpen: boolean;
  readonly paletteOpen: boolean;
  readonly changesWidth: number;
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
  readonly setPaletteOpen: (open: boolean) => void;
  readonly setChangesWidth: (width: number) => void;
  readonly setPlatform: (platform: AppPlatform, hostMenu: boolean) => void;
  readonly setNotice: (notice: string | null) => void;
  readonly setSession: (session: SessionActions | null) => void;
}

type DesktopLayoutStore = UseBoundStore<StoreApi<DesktopLayoutState>>;

export function clampChangesWidth(width: number): number {
  if (!Number.isFinite(width)) return CHANGES_WIDTH.initial;
  return Math.round(Math.min(CHANGES_WIDTH.max, Math.max(CHANGES_WIDTH.min, width)));
}

export function createDesktopLayoutStore(): DesktopLayoutStore {
  return create<DesktopLayoutState>((set) => ({
    sidebarOpen: true,
    changesOpen: true,
    paletteOpen: false,
    changesWidth: CHANGES_WIDTH.initial,
    platform: BROWSER_PLATFORM,
    hostMenu: false,
    notice: null,
    session: null,
    setSidebarOpen: (sidebarOpen) => set({ sidebarOpen }),
    setChangesOpen: (changesOpen) => set({ changesOpen }),
    setPaletteOpen: (paletteOpen) => set({ paletteOpen }),
    setChangesWidth: (width) => set({ changesWidth: clampChangesWidth(width) }),
    setPlatform: (platform, hostMenu) => set({ platform, hostMenu }),
    setNotice: (notice) => set({ notice }),
    setSession: (session) => set({ session }),
  }));
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
