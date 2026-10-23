/**
 * The typed client for the desktop host's background processes (#1223;
 * IPC.md "Background processes"): `process_*` commands and the
 * `process://event` channel. Wire shapes (`processTypes.ts`) are the host's
 * and are not renamed here; failures reject with a `WorkspaceIpcError`.
 */
import type { HostInvoke } from './hostBridge';
import type { ProcessEvent, ProcessInfo, ProcessOutput } from './processTypes';
import { tauriListen, toWorkspaceIpcError, type ListenFn, type TauriEventInternals } from './workspaceIpc';

export type { ProcessEvent, ProcessInfo } from './processTypes';

export interface ProcessIpc {
  /** The workspace session's processes, running and ended, oldest first; `[]` before it has a session. */
  list(workspaceId: string): Promise<ProcessInfo[]>;
  /** The output from byte `since` (default 0), at most 256 KiB: a pane opened late catches up. */
  read(workspaceId: string, processId: string, since?: number): Promise<ProcessOutput>;
  /** The person's keystrokes, sent as typed (at most 64 KiB). */
  write(workspaceId: string, processId: string, data: string): Promise<void>;
  /** A terminal process's size; a piped one ignores it. */
  resize(workspaceId: string, processId: string, rows: number, cols: number): Promise<void>;
  /** Stop its process group (`SIGTERM`, then `SIGKILL` after 2 s). */
  kill(workspaceId: string, processId: string): Promise<void>;
  /** A pinned process outlives its turn (until the session ends). */
  pin(workspaceId: string, processId: string, pinned: boolean): Promise<void>;
  /** Subscribe to `process://event`; resolves once the subscription is live, with its unsubscribe. */
  onEvent(handler: (event: ProcessEvent) => void): Promise<() => void>;
}

export const PROCESS_EVENT_CHANNEL = 'process://event';

export function createProcessIpc(hostInvoke: HostInvoke, listen: ListenFn): ProcessIpc {
  const invoke = <T>(command: string, args: Record<string, unknown>): Promise<T> =>
    hostInvoke<T>(command, args).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  const target = (workspaceId: string, processId: string): Record<string, unknown> => ({ workspace_id: workspaceId, process_id: processId });
  return {
    list: (workspaceId) => invoke<ProcessInfo[]>('process_list', { workspace_id: workspaceId }),
    read: (workspaceId, processId, since) =>
      invoke<ProcessOutput>('process_read', since === undefined ? target(workspaceId, processId) : { ...target(workspaceId, processId), since }),
    write: (workspaceId, processId, data) => invoke<void>('process_write', { ...target(workspaceId, processId), data }),
    resize: (workspaceId, processId, rows, cols) => invoke<void>('process_resize', { ...target(workspaceId, processId), rows, cols }),
    kill: (workspaceId, processId) => invoke<void>('process_kill', target(workspaceId, processId)),
    pin: (workspaceId, processId, pinned) => invoke<void>('process_pin', { ...target(workspaceId, processId), pinned }),
    onEvent: (handler) => listen(PROCESS_EVENT_CHANNEL, (payload) => handler(payload as ProcessEvent)),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriProcessIpc(): ProcessIpc | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  const bound: TauriEventInternals = {
    invoke: internals.invoke.bind(internals),
    transformCallback: internals.transformCallback.bind(internals),
  };
  return createProcessIpc(bound.invoke, tauriListen(bound));
}
