/**
 * The desktop host's language-server status (#1236; IPC.md "Language
 * servers"): the `lsp://event` channel. The servers behind edit diagnostics
 * start, stop and fail on the host; this only listens, for the composer's
 * chip. Wire shapes are the host's (snake_case).
 */
import { tauriListen, type ListenFn, type TauriEventInternals } from './workspaceIpc';

export type LspLanguage = 'rust' | 'typescript' | 'python' | 'go';

export type LspState = 'starting' | 'ready' | 'unavailable' | 'crashed' | 'stopped';

/** One `lsp://event`. */
export interface LspEvent {
  workspace_id: string;
  seq: number;
  kind: 'lsp_status';
  payload: { language: LspLanguage; state: LspState; message?: string };
}

export const LSP_EVENT_CHANNEL = 'lsp://event';

/** Subscribe with `listen`; resolves once the subscription is live, with its unsubscribe. */
export function onLspEvent(listen: ListenFn, handler: (event: LspEvent) => void): Promise<() => void> {
  return listen(LSP_EVENT_CHANNEL, (payload) => handler(payload as LspEvent));
}

/** The real listener, or `undefined` outside the Tauri webview. */
export function tauriLspListen(): ListenFn | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  return tauriListen({ invoke: internals.invoke.bind(internals), transformCallback: internals.transformCallback.bind(internals) });
}
