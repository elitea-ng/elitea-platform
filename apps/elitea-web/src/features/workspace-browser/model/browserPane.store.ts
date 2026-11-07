/**
 * What the window knows about each thread's browser pane, folded from
 * `browser://event`: whether it is open, the address and title, loading,
 * the last refusal, and the downloads (each one a card in the thread's
 * transcript). Kept for the session only; the page itself lives in the host.
 */
import { create, type StoreApi, type UseBoundStore } from 'zustand';

import type { BrowserControlState, BrowserEvent, BrowserRefusal } from '@/shared/desktop/browserIpc';

export interface BrowserDownload {
  readonly name: string;
  readonly size: number | null;
  readonly state: 'running' | 'done' | 'failed';
}

export interface PaneState {
  /** The pane is offered in the inspector (the person opened it). */
  readonly open: boolean;
  /** The host has a page for it (closed by the host after four live panes). */
  readonly live: boolean;
  readonly url: string;
  readonly title: string;
  readonly loading: boolean;
  readonly refused: { readonly url: string; readonly reason: BrowserRefusal; readonly message: string } | null;
  readonly downloads: readonly BrowserDownload[];
  /** Who drives the page, and the agent's request for the person (#1591). */
  readonly control: BrowserControlState;
}

const IDLE_CONTROL: BrowserControlState = { control: 'idle', reason: null, asked: false, watch: false };

export const EMPTY_PANE: PaneState = { open: false, live: false, url: '', title: '', loading: false, refused: null, downloads: [], control: IDLE_CONTROL };

/** The downloads a thread keeps on screen (the newest). */
const MAX_DOWNLOADS = 20;

interface BrowserPaneStore {
  readonly panes: Readonly<Record<string, PaneState>>;
  readonly update: (threadId: string, change: Partial<PaneState>) => void;
  readonly apply: (event: BrowserEvent) => void;
  /** The pane closed and its site data is gone (Forget site data, delete, archive). */
  readonly reset: (threadId: string) => void;
  /** Sign-out: nothing is kept. */
  readonly clear: () => void;
}

function withDownload(list: readonly BrowserDownload[], download: BrowserDownload): readonly BrowserDownload[] {
  const at = list.findIndex((item) => item.name === download.name);
  const next = at === -1 ? [...list, download] : list.map((item, index) => (index === at ? download : item));
  return next.slice(-MAX_DOWNLOADS);
}

/** One event applied to one pane: pure, so the store and its tests share it. */
export function foldBrowserEvent(pane: PaneState, event: BrowserEvent): PaneState {
  switch (event.kind) {
    case 'loading':
      return { ...pane, live: true, url: event.url, loading: true, refused: null };
    case 'loaded':
      return { ...pane, live: true, url: event.url, loading: false };
    case 'title':
      return { ...pane, title: event.title };
    case 'refused':
      return { ...pane, loading: false, refused: { url: event.url, reason: event.reason, message: event.message } };
    case 'download_started':
      return { ...pane, downloads: withDownload(pane.downloads, { name: event.name, size: null, state: 'running' }) };
    case 'download_finished':
      return { ...pane, downloads: withDownload(pane.downloads, { name: event.name, size: event.size, state: event.success ? 'done' : 'failed' }) };
    case 'control':
      return { ...pane, control: { control: event.control, reason: event.reason, asked: event.asked, watch: event.watch } };
    case 'closed':
      return { ...pane, live: false, loading: false };
  }
}

type Store = UseBoundStore<StoreApi<BrowserPaneStore>>;

function createBrowserPaneStore(): Store {
  return create<BrowserPaneStore>()((set) => ({
    panes: {},
    update: (threadId, change) => set((state) => ({ panes: { ...state.panes, [threadId]: { ...(state.panes[threadId] ?? EMPTY_PANE), ...change } } })),
    apply: (event) => set((state) => ({ panes: { ...state.panes, [event.thread_id]: foldBrowserEvent(state.panes[event.thread_id] ?? EMPTY_PANE, event) } })),
    reset: (threadId) =>
      set((state) => {
        const { [threadId]: _gone, ...rest } = state.panes;
        return { panes: rest };
      }),
    clear: () => set({ panes: {} }),
  }));
}

// R-S2: built on first use, not at module scope (see `widgets/sidebar/model/sidebarCollapsed.store.ts`).
let instance: Store | undefined;
const resolveStore = (): Store => (instance ??= createBrowserPaneStore());

function useBrowserPanesHook<T>(selector: (state: BrowserPaneStore) => T): T {
  return resolveStore()(selector);
}

/** The window's one pane store: a hook, plus `getState` for handlers outside React. */
export const useBrowserPanes = Object.assign(useBrowserPanesHook, {
  getState: (): BrowserPaneStore => resolveStore().getState(),
});

/** One thread's pane (the empty one when it never opened). */
export function useBrowserPane(threadId: string): PaneState {
  return useBrowserPanes((state) => state.panes[threadId] ?? EMPTY_PANE);
}
