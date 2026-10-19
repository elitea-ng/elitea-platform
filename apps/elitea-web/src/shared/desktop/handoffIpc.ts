/**
 * The typed client for the desktop host's cloud handoff IPC (IPC.md, "Cloud
 * handoff"; ADR-0034): `handoff_*` commands and the `handoff://event`
 * progress channel. Wire shapes are the host's (snake_case) and are not
 * renamed here. Failures reject with a `HandoffIpcError`: the host's
 * `{code, message, details}`, where `details` carries the secret findings
 * (`secret_found`, `include_confirm_required`), the cap breakdown (`handoff_too_large`) or the server's
 * quota figures.
 */
import type { HostInvoke } from './hostBridge';
import { tauriListen, WorkspaceIpcError, type ChangedFile, type ListenFn, type TauriEventInternals } from './workspaceIpc';

/**
 * Why a path is not sent: the platform's or the project's exclusions, the
 * person's own folder patterns, `.eliteaignore`, the organisation's hard
 * deny list, left on the laptop for this send, or not a regular file.
 */
export type ExcludeReason = 'platform' | 'project' | 'user' | 'eliteaignore' | 'path_deny' | 'leave_on_laptop' | 'not_a_file';

export interface ExcludedPath {
  path: string;
  reason: ExcludeReason;
  tracked: boolean;
  /** The policy allows including it and it is not `path_deny`, left on the laptop or not a file. */
  includable: boolean;
}

/** An excluded path the folder's settings include: it is sent. */
export interface IncludedPath {
  path: string;
  reason: ExcludeReason;
}

/** An environment variable the desktop could send: its name and how long its value is. The value never reaches the UI. */
export interface EnvVar {
  name: string;
  length: number;
}

/** The effective handoff policy (the server's, or the ADR's defaults when the server has no policy route). */
export interface HandoffPolicy {
  /** `defaults`: the server did not answer the policy route (an older server); the ADR's defaults are shown. */
  source: 'server' | 'defaults';
  exclusions: { platform: string[]; project: string[]; user: string[]; path_deny: string[] };
  allow_user_include: boolean;
  env_harvest: boolean;
  env_limits: { max_vars: number; max_bytes: number };
  limits: { max_segment_bytes: number; max_file_bytes: number; max_files: number; max_unpacked_bytes: number; project_workspace_bytes_left: number | null };
}

/** One folder's own handoff settings, kept in the app data (never in the folder). */
export interface FolderHandoffSettings {
  /** Matched at any depth; a leading `/` anchors at the top. */
  exclude_patterns: string[];
  /** An exact path, or a pattern anchored at the top. */
  include: string[];
  /** Variable NAMES only; values are read again at send and never stored. */
  env_names: string[];
}

/** `handoff_settings_get` / `handoff_settings_set`. */
export interface HandoffSettingsAnswer {
  settings: FolderHandoffSettings;
  policy: HandoffPolicy;
  env_offered: EnvVar[];
  /** The login shell could not be read (failed or timed out): nothing is offered. */
  env_notice: string | null;
}

/** The env harvest part of a preview; `null` on the preview when the policy has no env harvest. */
export interface PreviewEnv {
  offered: EnvVar[];
  /** The names in the folder's settings. */
  selected: string[];
  /** Selected, but not in the environment now. */
  missing: string[];
  vars: number;
  bytes: number;
  over_limit: boolean;
  /** The login shell could not be read (failed or timed out): nothing is offered. */
  notice: string | null;
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
  /** Sent because the folder's settings include them. */
  included: IncludedPath[];
  /** Outside included files: they block the send. */
  findings: SecretFinding[];
  /** In included files: warnings; the send needs `confirm_includes`. */
  include_findings: SecretFinding[];
  has_upstream: boolean;
  /** From the policy route; `null` when the server has none. */
  project_workspace_bytes_left: number | null;
  caps: CapsBreakdown;
  /** The folder is not a repository: a shadow git directory in the app data is used. */
  shadow: boolean;
  policy: HandoffPolicy;
  /** `null` when the policy has no env harvest. */
  env: PreviewEnv | null;
}

type HandoffPhase = 'snapshot' | 'scan' | 'bundle' | 'begin' | 'upload' | 'publish' | 'complete' | 'cancelled' | 'error';

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
  included: number;
  env_vars: number;
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
  /** Per-file diffs of what the take back writes (text files only, 256 KiB of diff text at most). */
  files: HandoffChangedFile[];
  diffs_truncated: boolean;
}

/** A file the take back writes; the shape of the Workspace view's changed file. */
export type HandoffChangedFile = Omit<ChangedFile, 'status'> & { status: 'added' | 'modified' | 'deleted' };

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
  /**
   * The server refused the release for good (`handoff_erased`: the cloud copy was erased meanwhile;
   * `stale_epoch`, `handoff_not_found`). Not sent again; the thread follows the server's state.
   */
  release_refused?: string | null;
  /** The folder's excluded files (`.env`, keys) copied into a worktree. */
  copied: string[];
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
  /** `confirmIncludes`: the person confirmed the included files' findings for this send. */
  send(workspaceId: string, conversationUuid: string, leaveOnLaptop: readonly string[], options?: { confirmIncludes?: boolean }): Promise<HandoffSent>;
  /** Stop a running send; the thread is `LOCAL` again and the send rejects `handoff_cancelled`. */
  cancel(conversationUuid: string): Promise<{ cancelled: boolean }>;
  /** `projectId`: for a thread this desktop holds no record of. */
  status(conversationUuid: string, projectId?: number): Promise<HandoffStatus>;
  /** Many threads at once (the sidebar), by UUID; from the host's records. */
  statusMany(conversationUuids: readonly string[]): Promise<Record<string, HandoffStatus>>;
  settingsGet(workspaceId: string): Promise<HandoffSettingsAnswer>;
  /** Rejects `invalid_request` for a bad pattern, an include that escapes the folder, or a bad env name. */
  settingsSet(workspaceId: string, settings: FolderHandoffSettings): Promise<HandoffSettingsAnswer>;
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
    send: (workspaceId, uuid, leave, options) =>
      invoke<HandoffSent>('handoff_send', {
        workspace_id: workspaceId,
        conversation_uuid: uuid,
        leave_on_laptop: [...leave],
        ...(options?.confirmIncludes === true ? { confirm_includes: true } : {}),
      }),
    cancel: (uuid) => invoke<{ cancelled: boolean }>('handoff_cancel', { conversation_uuid: uuid }),
    status: (uuid, projectId) => invoke<HandoffStatus>('handoff_status', withProject(uuid, projectId)),
    statusMany: async (uuids) =>
      uuids.length === 0
        ? {}
        : (await invoke<{ statuses: Record<string, HandoffStatus> }>('handoff_status_many', { conversation_uuids: [...uuids] })).statuses,
    settingsGet: (workspaceId) => invoke<HandoffSettingsAnswer>('handoff_settings_get', { workspace_id: workspaceId }),
    settingsSet: (workspaceId, settings) => invoke<HandoffSettingsAnswer>('handoff_settings_set', { workspace_id: workspaceId, settings }),
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
