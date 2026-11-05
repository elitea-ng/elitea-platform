/**
 * The typed client for the desktop host's browser pane (ADR-0048; IPC.md,
 * "Browser pane"): one page per Local work thread, a native child webview
 * the host lays over the rectangle this page reports. The page in it has no
 * IPC; only this (the bundled UI) drives it. Wire shapes are the host's
 * (snake_case). Failures reject with a `WorkspaceIpcError`.
 */
import { tauriInvoke, type HostInvoke } from './hostBridge';
import { tauriListen, toWorkspaceIpcError, type ListenFn, type TauriEventInternals } from './workspaceIpc';

/** The page rectangle, in the main page's CSS pixels (the host scales it by the app's zoom). */
export interface PaneRect {
  x: number;
  y: number;
  width: number;
  height: number;
}

export type BrowserGoAction = 'back' | 'forward' | 'reload' | 'stop';

/** Why the guard refused a navigation. */
export type BrowserRefusal = 'scheme' | 'http' | 'private_network' | 'forbidden' | 'credentials';

/** One `browser://event`. */
export type BrowserEvent =
  | { thread_id: string; kind: 'loading' | 'loaded'; url: string }
  | { thread_id: string; kind: 'title'; title: string }
  | { thread_id: string; kind: 'refused'; url: string; reason: BrowserRefusal; message: string }
  | { thread_id: string; kind: 'download_started'; name: string }
  | { thread_id: string; kind: 'download_finished'; name: string; size: number | null; success: boolean }
  /** The host closed the pane to keep at most four alive; the next open makes a new page. */
  | { thread_id: string; kind: 'closed' };

const BROWSER_EVENT_CHANNEL = 'browser://event';

export interface BrowserIpc {
  /** Open (or re-point) the thread's page at `address` (`example.com` means https). Resolves with the URL it loads. */
  open(threadId: string, address: string, rect: PaneRect): Promise<{ url: string }>;
  /** The page rectangle moved, or the page is to be hidden (another tab, a modal sheet) or shown again. */
  setRect(threadId: string, rect: PaneRect, visible: boolean): Promise<void>;
  go(threadId: string, action: BrowserGoAction): Promise<void>;
  /** Close the page; its site data stays. */
  close(threadId: string): Promise<void>;
  /** Forget site data: the page closes and the thread's cookies and storage are deleted. */
  forget(threadId: string): Promise<boolean>;
  /** Show a finished download in Finder. */
  revealDownload(threadId: string, name: string): Promise<void>;
  /** Subscribe to `browser://event`; resolves once live, with its unsubscribe. */
  onEvent(handler: (event: BrowserEvent) => void): Promise<() => void>;
}

function createBrowserIpc(hostInvoke: HostInvoke, listen: ListenFn): BrowserIpc {
  const invoke = <T>(command: string, args: Record<string, unknown>): Promise<T> =>
    hostInvoke<T>(command, args).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  return {
    open: (threadId, address, rect) => invoke<{ url: string }>('browser_open', { thread_id: threadId, address, rect }),
    setRect: (threadId, rect, visible) => invoke<void>('browser_set_rect', { thread_id: threadId, rect, visible }),
    go: (threadId, action) => invoke<void>('browser_go', { thread_id: threadId, action }),
    close: (threadId) => invoke<void>('browser_close', { thread_id: threadId }),
    forget: async (threadId) => (await invoke<{ forgotten: boolean }>('browser_forget', { thread_id: threadId })).forgotten,
    revealDownload: (threadId, name) => invoke<void>('browser_reveal_download', { thread_id: threadId, name }),
    onEvent: (handler) => listen(BROWSER_EVENT_CHANNEL, (payload) => handler(payload as BrowserEvent)),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriBrowserIpc(): BrowserIpc | undefined {
  const invoke = tauriInvoke();
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (invoke === undefined || internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  return createBrowserIpc(invoke, tauriListen({ invoke: internals.invoke.bind(internals), transformCallback: internals.transformCallback.bind(internals) }));
}
