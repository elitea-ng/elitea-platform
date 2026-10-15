/**
 * The typed client for the desktop host's cloud handoff IPC (IPC.md, "Cloud
 * handoff"; ADR-0034): `handoff_*` commands and the `handoff://event`
 * progress channel. Wire shapes are the host's (snake_case) and are not
 * renamed here. Failures reject with a `HandoffIpcError`: the host's
 * `{code, message, details}`, where `details` carries the secret findings
 * (`secret_found`), the cap breakdown (`handoff_too_large`) or the server's
 * quota figures.
 */
import type { HostInvoke } from './hostBridge';
import { tauriListen, WorkspaceIpcError, type ListenFn, type TauriEventInternals } from './workspaceIpc';

export type ExcludeReason = 'mandatory' | 'path_deny' | 'eliteaignore' | 'leave_on_laptop' | 'not_a_file';

interface ExcludedPath {
  path: string;
  reason: ExcludeReason;
  tracked: boolean;
}

/** A credential-shaped line. Never the matched text. */
export interface SecretFinding {
  path: string;
  line: number;
  rule: string;
}

interface CapViolation {
  /** Which cap: a segment, a file, the file count or the unpacked tree. */
  cap: string;
  limit: number;
  actual: number;
  path: string | null;
}

export interface CapsBreakdown {
  violations: CapViolation[];
  largest: { path: string; bytes: number }[];
}

/** `handoff_preview`: what a send would transfer; nothing is sent. */
export interface HandoffPreview {
  files: number;
  bytes: number;
  unpacked_bytes: number;
  excluded: ExcludedPath[];
  findings: SecretFinding[];
  has_upstream: boolean;
  /** `null`: known only after the begin call. */
  project_workspace_bytes_left: number | null;
  caps: CapsBreakdown;
  /** The folder is not a repository: a shadow git directory in the app data is used. */
  shadow: boolean;
}

type HandoffPhase = 'snapshot' | 'scan' | 'bundle' | 'upload' | 'publish' | 'complete' | 'error';

export interface HandoffEvent {
  conversation_uuid: string;
  phase: HandoffPhase;
  done: number;
  total: number;
  message: string | null;
}

export type HandoffEventHandler = (event: HandoffEvent) => void;

export interface HandoffSent {
  handoff_id: string;
  state: string;
  epoch: number;
  files: number;
  excluded: number;
}

export type HandoffExecutor = 'local' | 'cloud' | 'returning';

/** The server's banner state (contract 2.9) plus what the host adds. */
export interface HandoffStatus {
  /** `LOCAL`, `HANDING_OFF`, `CLOUD_IDLE`, `CLOUD_RUNNING` or `RECLAIMING`. */
  state: string;
  epoch: number;
  executor: HandoffExecutor;
  read_only: boolean;
  /** This desktop holds the record of the send; a take back is possible only here. */
  sent_from_here: boolean;
  reclaim_pending?: boolean;
  handoff_id?: string;
  device_name?: string;
  history_mode?: string;
  has_upstream?: boolean;
  cloud_sha?: string | null;
  updated_at?: string;
  delete_after?: string | null;
  workspace_bytes?: number;
}

/** `handoff_reclaim`: `RECLAIMING`, or `CLOUD_RUNNING` with `reclaim_pending` while a cloud turn runs. */
export interface ReclaimAnswer {
  state: string;
  epoch: number;
  reclaim_pending?: boolean;
  cloud_sha?: string | null;
}

export type TakeBackMode = 'apply' | 'worktree' | 'discard';

export interface Preflight {
  preflight_id: string;
  outcome: 'fast_path' | 'conflicts';
  changed_paths: string[];
  conflict_paths: string[];
  /** Excluded paths, symlinks and submodule entries: never written or deleted. */
  skipped_paths: string[];
  cloud_turns: number;
  /** What `handoff_apply` accepts now: `apply` only on the fast path. */
  modes: TakeBackMode[];
}

export interface ApplyResult {
  applied: boolean;
  outcome: 'applied' | 'worktree' | 'discarded';
  /** `handoff_undo` restores it; `null` when the folder was not written. */
  checkpoint_id: string | null;
  worktree_path: string | null;
  branch: string | null;
  imported_turns: number;
  /** The release was acknowledged; `false`: kept and sent again with the next status. */
  released: boolean;
}

export class HandoffIpcError extends WorkspaceIpcError {
  readonly details: unknown;

  constructor(code: string, message: string, details: unknown = null) {
    super(code, message);
    this.name = 'HandoffIpcError';
    this.details = details;
  }
}

export function toHandoffIpcError(error: unknown): HandoffIpcError {
  if (error instanceof HandoffIpcError) return error;
  if (typeof error === 'object' && error !== null) {
    const { code, message, details } = error as { code?: unknown; message?: unknown; details?: unknown };
    if (typeof code === 'string' && typeof message === 'string') return new HandoffIpcError(code, message, details ?? null);
  }
  if (typeof error === 'string') return new HandoffIpcError('unknown', error);
  return new HandoffIpcError('unknown', error instanceof Error ? error.message : 'Something went wrong.');
}

export interface HandoffIpc {
  /** The device policy allows a send (`local_work.allowed` and `local_work.cloud_handoff`); the Send button is shown only then. */
  allowed(): Promise<boolean>;
  /** What a send would transfer. `leaveOnLaptop` paths are left out of it. */
  preview(workspaceId: string, conversationUuid: string, leaveOnLaptop?: readonly string[]): Promise<HandoffPreview>;
  send(workspaceId: string, conversationUuid: string, leaveOnLaptop: readonly string[]): Promise<HandoffSent>;
  /** `projectId`: for a thread this desktop holds no record of. */
  status(conversationUuid: string, projectId?: number): Promise<HandoffStatus>;
  reclaim(conversationUuid: string): Promise<ReclaimAnswer>;
  preflight(conversationUuid: string): Promise<Preflight>;
  apply(preflightId: string, mode: TakeBackMode): Promise<ApplyResult>;
  /** Restore the folder to before a take back. */
  undo(checkpointId: string): Promise<{ restored: string[] }>;
  /** Erase the cloud copy; the local folder is kept. */
  erase(conversationUuid: string, projectId?: number): Promise<unknown>;
  /** Subscribe to `handoff://event`; resolves once the subscription is live, with its unsubscribe. */
  onEvent(handler: HandoffEventHandler): Promise<() => void>;
}

export const HANDOFF_EVENT_CHANNEL = 'handoff://event';

/** `local_work.allowed && local_work.cloud_handoff` of the stored `native_client_policy`. */
export function handoffAllowedBy(policy: Record<string, unknown> | null): boolean {
  const section = policy?.['local_work'];
  if (typeof section !== 'object' || section === null) return false;
  const { allowed, cloud_handoff: cloudHandoff } = section as Record<string, unknown>;
  return allowed === true && cloudHandoff === true;
}

export function createHandoffIpc(hostInvoke: HostInvoke, listen: ListenFn): HandoffIpc {
  const invoke = <T>(command: string, args?: Record<string, unknown>): Promise<T> =>
    (args === undefined ? hostInvoke<T>(command) : hostInvoke<T>(command, args)).catch((error: unknown) => {
      throw toHandoffIpcError(error);
    });
  const withProject = (uuid: string, projectId: number | undefined): Record<string, unknown> =>
    projectId === undefined ? { conversation_uuid: uuid } : { conversation_uuid: uuid, project_id: projectId };
  return {
    allowed: async () => handoffAllowedBy((await invoke<{ policy: Record<string, unknown> | null }>('host_state')).policy),
    preview: (workspaceId, uuid, leave) =>
      invoke<HandoffPreview>('handoff_preview', {
        workspace_id: workspaceId,
        conversation_uuid: uuid,
        ...(leave === undefined || leave.length === 0 ? {} : { leave_on_laptop: [...leave] }),
      }),
    send: (workspaceId, uuid, leave) =>
      invoke<HandoffSent>('handoff_send', { workspace_id: workspaceId, conversation_uuid: uuid, leave_on_laptop: [...leave] }),
    status: (uuid, projectId) => invoke<HandoffStatus>('handoff_status', withProject(uuid, projectId)),
    reclaim: (uuid) => invoke<ReclaimAnswer>('handoff_reclaim', { conversation_uuid: uuid }),
    preflight: (uuid) => invoke<Preflight>('handoff_preflight', { conversation_uuid: uuid }),
    apply: (preflightId, mode) => invoke<ApplyResult>('handoff_apply', { preflight_id: preflightId, mode }),
    undo: (checkpointId) => invoke<{ restored: string[] }>('handoff_undo', { checkpoint_id: checkpointId }),
    erase: (uuid, projectId) => invoke<unknown>('handoff_erase', withProject(uuid, projectId)),
    onEvent: (handler) => listen(HANDOFF_EVENT_CHANNEL, (payload) => handler(payload as HandoffEvent)),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriHandoffIpc(): HandoffIpc | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  const bound: TauriEventInternals = {
    invoke: internals.invoke.bind(internals),
    transformCallback: internals.transformCallback.bind(internals),
  };
  return createHandoffIpc(bound.invoke, tauriListen(bound));
}
