/**
 * The typed client for the desktop host's local workspace index (IPC.md,
 * "Local index"; ADR-0029 decision 7): the `index_*` commands and the
 * `index://event` progress channel. Wire shapes are the host's (snake_case)
 * and are not renamed here. Failures reject with a `WorkspaceIpcError`
 * (`local_index_disabled`, `index_busy`, `index_refused`, …).
 */
import type { HostInvoke } from './hostBridge';
import { tauriListen, toWorkspaceIpcError, type ListenFn, type TauriEventInternals } from './workspaceIpc';

export type IndexState = 'off' | 'building' | 'ready' | 'stale' | 'error';

export interface IndexStatus {
  state: IndexState;
  /** Documents of the last build. */
  files: number;
  entities: number;
  relations: number;
  /** 0 until embeddings exist. */
  vectors: number;
  /** When the last build was committed (UTC, ISO 8601). */
  last_run: string | null;
  /** Files turns changed since the last build. */
  changed_files: number;
  /** Why the last refresh failed. */
  error: string | null;
}

export type IndexPhase = 'listing' | 'parsing' | 'building' | 'saving' | 'ready' | 'stale' | 'cancelled' | 'error';

export interface IndexEvent {
  workspace_id: string;
  phase: IndexPhase;
  /** Progress text, or why it failed. */
  message: string | null;
  /** As `index_status` would answer now. */
  status: IndexStatus;
}

export type IndexEventHandler = (event: IndexEvent) => void;

export interface IndexIpc {
  status(workspaceId: string): Promise<IndexStatus>;
  /** Turn the index on and start building it. */
  enable(workspaceId: string): Promise<IndexStatus>;
  /** Stop building and stop offering the index; the build is kept. */
  disable(workspaceId: string): Promise<IndexStatus>;
  /** Start a refresh; `full` rebuilds from nothing. */
  refresh(workspaceId: string, full?: boolean): Promise<IndexStatus>;
  /** Stop the running refresh; `false` when none runs. */
  cancel(workspaceId: string): Promise<boolean>;
  /** Delete the index (the person confirmed). */
  remove(workspaceId: string): Promise<void>;
  /** Subscribe to `index://event`; resolves once the subscription is live, with its unsubscribe. */
  onEvent(handler: IndexEventHandler): Promise<() => void>;
}

export const INDEX_EVENT_CHANNEL = 'index://event';

/** The status of an index that is not turned on (what `index_disable` answers). */
export const OFF_STATUS: IndexStatus = Object.freeze({
  state: 'off',
  files: 0,
  entities: 0,
  relations: 0,
  vectors: 0,
  last_run: null,
  changed_files: 0,
  error: null,
});

export function createIndexIpc(hostInvoke: HostInvoke, listen: ListenFn): IndexIpc {
  const invoke = <T>(command: string, args: Record<string, unknown>): Promise<T> =>
    hostInvoke<T>(command, args).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  return {
    status: (workspaceId) => invoke<IndexStatus>('index_status', { workspace_id: workspaceId }),
    enable: (workspaceId) => invoke<IndexStatus>('index_enable', { workspace_id: workspaceId }),
    disable: (workspaceId) => invoke<IndexStatus>('index_disable', { workspace_id: workspaceId }),
    refresh: (workspaceId, full) =>
      invoke<IndexStatus>('index_refresh', full === true ? { workspace_id: workspaceId, full: true } : { workspace_id: workspaceId }),
    cancel: (workspaceId) => invoke<boolean>('index_cancel', { workspace_id: workspaceId }),
    remove: (workspaceId) => invoke<void>('index_remove', { workspace_id: workspaceId, confirm: true }),
    onEvent: (handler) => listen(INDEX_EVENT_CHANNEL, (payload) => handler(payload as IndexEvent)),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriIndexIpc(): IndexIpc | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  const bound: TauriEventInternals = {
    invoke: internals.invoke.bind(internals),
    transformCallback: internals.transformCallback.bind(internals),
  };
  return createIndexIpc(bound.invoke, tauriListen(bound));
}
