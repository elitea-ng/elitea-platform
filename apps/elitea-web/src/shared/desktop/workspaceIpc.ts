/**
 * The typed client for the desktop host's workspace + agent-turn IPC
 * (ADR-0029 D0). Commands go through the same `invoke` seam as `hostBridge`;
 * turn progress arrives on one event channel, `agent://event`.
 *
 * The wire shapes are the host's contract (snake_case, exactly as the Rust
 * side serialises them) and are NOT renamed here: a rename layer is one more
 * place for the two halves to drift apart.
 */
import type { HostInvoke } from './hostBridge';

export interface Workspace {
  id: string;
  path: string;
  name: string;
  project_id: number | null;
  is_git: boolean;
}

export interface TurnStartRequest {
  workspace_id: string;
  project_id: number;
  conversation_id: string;
  application_id: number;
  version_id: number;
  prompt: string;
  plan_mode: boolean;
  /** Workspace-relative paths the person referenced with "@" (a folder ends with `/`); the host checks them and lists them under the prompt. */
  mentions: string[];
  /** The agent's skills the person picked with "/" (by name); the host resolves them against the agent version and applies them to this turn. */
  skills: string[];
}

/** `agent_prewarm`: the agent version (and thread) the composer is about to send to. */
export interface TurnPrewarmRequest {
  project_id: number;
  application_id: number;
  version_id: number;
  /** The thread's conversation, when it has one. */
  conversation_id?: string;
}

/** What a turn to it would find cached on the host now. */
interface TurnPrewarmed {
  version: boolean;
  participant: boolean;
}

/** One match of `workspace_files` (the "@" picker). */
export interface WorkspaceFile {
  /** Workspace-relative, without a trailing `/`. */
  path: string;
  kind: 'file' | 'dir';
}

export interface TurnStarted {
  turn_id: string;
  execution_id: string;
}

export type ApprovalDecision = 'allow_once' | 'allow_always' | 'deny';
type ChangeStatus = 'added' | 'modified' | 'deleted' | 'renamed';

export interface ChangedFile {
  path: string;
  status: ChangeStatus;
  added: number;
  removed: number;
  /** Unified diff text; may be empty for binary or very large files. */
  diff: string;
}

export interface TurnChanges {
  files: ChangedFile[];
  /** The newest turn of its folder whose changes stand: "Undo turn" is for it only; an older turn offers "Restore folder to before this turn". */
  latest: boolean;
  /** Its changes were undone (its own undo, or the folder restored to before an earlier turn): nothing to undo. */
  undone: boolean;
}

/** `checkpoint_preview`: what restoring the folder to before a turn would do now (later turns and the person's own edits included). */
export interface RestorePreview {
  /** Written back to how they were before the turn. */
  restored: string[];
  /** Deleted: they did not exist before the turn. */
  deleted: string[];
}

interface RestoreOptions {
  /** Restore the folder to before an OLDER turn (the person confirmed the preview); without it the host refuses with `undo_not_latest`. */
  confirmOlder?: boolean;
}

export type TurnPhase = 'resolving' | 'starting' | 'running' | 'committing' | 'done' | 'cancelled' | 'error';

export interface ApprovalRequestPayload {
  request_id: string;
  /** `continue`: the run used its steps; asked every time, shown as a card in the transcript. Absent for a tool's approval. */
  kind?: 'continue';
  /** For `continue`: the steps each run takes (and the next one would). */
  steps?: number;
  tool: string;
  title: string;
  detail: string;
  command?: string[];
  paths?: string[];
  reason: string;
  can_remember: boolean;
}

interface EventBase {
  turn_id: string;
  seq: number;
}

/** The `done` event's payload. */
export interface TurnDonePayload {
  /** True once the turn is saved in its conversation: `done` comes before the commit, so the event says `false` and `agent_turn_status` says how it settled. */
  committed: boolean;
  /** `pending` (being saved after `done`), `committed`, `failed` (see `commit_failed`) or `none` (a cancelled turn saves nothing). */
  commit?: 'pending' | 'committed' | 'failed' | 'none';
  execution_id?: string;
  conversation_id: string;
  message_ids: string[];
  changed_files: number;
}

/** `agent_turn_status`: where a turn is; `done` is the `done` event's payload once it was sent. */
export interface TurnStatus {
  state: 'running' | 'committing' | 'done';
  done: TurnDonePayload | null;
}

export type AgentEvent =
  | (EventBase & {
      kind: 'status';
      payload: {
        phase: TurnPhase;
        message?: string;
        project_instructions?: string[];
        /** On `running` again: the run went on past its step limit, for the n-th time, from the host's own `host_prompt` (never the person's message). */
        continued?: number;
        host_prompt?: string;
        /** On `done`: `step_limit_reached` when the turn stopped at its limit. */
        note?: string;
        /** The turn's phases measured since the previous `status` (IPC.md "Timings"). */
        timings?: { phase: string; ms_since_turn_start: number; phase_ms: number }[];
      };
    })
  | (EventBase & { kind: 'text_delta'; payload: { text: string } })
  | (EventBase & { kind: 'tool_call'; payload: { call_id: string; tool: string; args_summary: string; remote: boolean } })
  | (EventBase & { kind: 'tool_result'; payload: { call_id: string; ok: boolean; summary: string; truncated: boolean } })
  /** One model call's usage: `prompt_tokens` is what it sent (the context in use), `completion_tokens` what it produced. Absent when the gateway reports no usage. */
  | (EventBase & { kind: 'usage'; payload: { prompt_tokens: number; completion_tokens: number; total_tokens: number } })
  | (EventBase & { kind: 'approval_request'; payload: ApprovalRequestPayload })
  | (EventBase & { kind: 'error'; payload: { code: string; message: string } })
  | (EventBase & { kind: 'done'; payload: TurnDonePayload })
  /** After `done`: the turn's commit did not land after its retries; `turn_commit_retry` sends it again. */
  | (EventBase & { kind: 'commit_failed'; payload: { execution_id: string; code: string; message: string } });

export type AgentEventHandler = (event: AgentEvent) => void;

/** One recorded turn of a thread (`thread_history`, IPC.md "Thread history"). */
export interface StoredTurn {
  turn_id: string;
  conversation_id: string;
  conversation_uuid: string | null;
  /** As typed; the "@" references are `mentions`. */
  prompt: string;
  mentions: string[];
  /** Unix ms. */
  started_at: number;
  finished_at: number | null;
  /** The turn's `agent://event` stream: fold it like the live one. */
  events: AgentEvent[];
  /** What the turn changed, as it ended (`null` before it ended). */
  changes: ChangedFile[] | null;
  events_truncated: boolean;
  /** `interrupted`: it never ended (the app quit while it ran). */
  state: 'done' | 'running' | 'interrupted';
  /** The host still keeps it: `turn_changes` / `checkpoint_restore` answer for it. */
  live: boolean;
}

/** What `thread_history_fork` copies (IPC.md "Thread history"). */
export interface ThreadForkRequest {
  workspaceId: string;
  sourceConversationId: string;
  targetConversationId: string;
  targetConversationUuid: string;
  /** The fork point's turn: the turns up to and including it are copied. Absent: every turn. */
  upToTurnId?: string;
  /**
   * The fork answer's `forked_messages` (source message UUID → the copy's): the copied turns' `done`
   * events name the new conversation's messages, so they can be fork points in turn.
   */
  messageIds?: Readonly<Record<string, string>>;
}

/**
 * A rejected workspace / turn command. The host rejects with `{code, message}`
 * (`IpcError` in `src-tauri/src/local_commands.rs`): `code` is what the UI
 * branches on (`workspace_busy`, `agent_version_mismatch`, …), `message` is
 * written for a person. Anything else the seam throws gets code `unknown`.
 */
export class WorkspaceIpcError extends Error {
  readonly code: string;

  constructor(code: string, message: string) {
    super(message);
    this.name = 'WorkspaceIpcError';
    this.code = code;
  }
}

export function toWorkspaceIpcError(error: unknown): WorkspaceIpcError {
  if (error instanceof WorkspaceIpcError) return error;
  if (typeof error === 'object' && error !== null) {
    const { code, message } = error as { code?: unknown; message?: unknown };
    if (typeof code === 'string' && typeof message === 'string') return new WorkspaceIpcError(code, message);
  }
  if (typeof error === 'string') return new WorkspaceIpcError('unknown', error);
  return new WorkspaceIpcError('unknown', error instanceof Error ? error.message : 'Something went wrong.');
}

export interface WorkspaceIpc {
  open(): Promise<Workspace | null>;
  list(): Promise<Workspace[]>;
  remove(id: string): Promise<void>;
  bindProject(id: string, projectId: number): Promise<void>;
  /** The "@" picker: the workspace's files and folders matching `query`, best first. */
  files(workspaceId: string, query: string, limit?: number): Promise<WorkspaceFile[]>;
  startTurn(request: TurnStartRequest): Promise<TurnStarted>;
  /** Send a turn's failed commit again (after `commit_failed`); resolves once it is saved. */
  retryCommit(executionId: string): Promise<void>;
  /** Have the host read ahead what a turn to this agent reads first (the composer got focus); starts nothing. */
  prewarm(request: TurnPrewarmRequest): Promise<TurnPrewarmed>;
  cancelTurn(turnId: string): Promise<void>;
  /** Where a turn is, for a UI that may have missed its `done` event (events are not replayed). */
  turnStatus(turnId: string): Promise<TurnStatus>;
  respondApproval(requestId: string, decision: ApprovalDecision): Promise<void>;
  turnChanges(turnId: string): Promise<TurnChanges>;
  /** Undo the newest turn (or one file of a turn), or with `confirmOlder` restore the folder to before an older turn. */
  restore(turnId: string, path?: string, options?: RestoreOptions): Promise<{ restored: string[] }>;
  /** What restoring the folder to before the turn would write back and delete now; changes nothing. */
  restorePreview(turnId: string): Promise<RestorePreview>;
  /** The turns this machine recorded in a thread, oldest first; `[]` when it recorded none. */
  threadHistory(workspaceId: string, conversationId: string): Promise<StoredTurn[]>;
  /** Forget a thread's recorded turns (the conversation itself stays); resolves with how many. */
  deleteThreadHistory(workspaceId: string, conversationId: string): Promise<number>;
  /**
   * The local half of a fork (`thread_history_fork`): copy a thread's recorded turns, up to and including
   * `upToTurnId` (all when absent), into the conversation the server fork made, in the same folder.
   * Resolves with how many turns were copied (0 when this machine recorded none).
   */
  forkThreadHistory(request: ThreadForkRequest): Promise<number>;
  /** Subscribe to `agent://event`; resolves once the subscription is live, with its unsubscribe. */
  onEvent(handler: AgentEventHandler): Promise<() => void>;
}

const AGENT_EVENT_CHANNEL = 'agent://event';

/** What the Tauri event plugin delivers to a registered callback. */
interface TauriEnvelope {
  event: string;
  id: number;
  payload: unknown;
}

/** The event-plugin half of the seam: register a callback, get back an unsubscribe. */
export type ListenFn = (channel: string, handler: (payload: unknown) => void) => Promise<() => void>;

export function createWorkspaceIpc(hostInvoke: HostInvoke, listen: ListenFn): WorkspaceIpc {
  const invoke = <T>(command: string, args?: Record<string, unknown>): Promise<T> =>
    (args === undefined ? hostInvoke<T>(command) : hostInvoke<T>(command, args)).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  return {
    open: () => invoke<Workspace | null>('workspace_open'),
    list: () => invoke<Workspace[]>('workspace_list'),
    remove: (id) => invoke<void>('workspace_remove', { id }),
    bindProject: (id, projectId) => invoke<void>('workspace_bind_project', { id, project_id: projectId }),
    files: (workspaceId, query, limit) =>
      invoke<WorkspaceFile[]>('workspace_files', limit === undefined ? { workspace_id: workspaceId, query } : { workspace_id: workspaceId, query, limit }),
    startTurn: (request) => invoke<TurnStarted>('agent_turn_start', { ...request }),
    prewarm: (request) => invoke<TurnPrewarmed>('agent_prewarm', { ...request }),
    retryCommit: (executionId) => invoke<void>('turn_commit_retry', { execution_id: executionId }),
    cancelTurn: (turnId) => invoke<void>('agent_turn_cancel', { turn_id: turnId }),
    turnStatus: (turnId) => invoke<TurnStatus>('agent_turn_status', { turn_id: turnId }),
    respondApproval: (requestId, decision) => invoke<void>('approval_respond', { request_id: requestId, decision }),
    turnChanges: (turnId) => invoke<TurnChanges>('turn_changes', { turn_id: turnId }),
    restore: (turnId, path, options) =>
      invoke<{ restored: string[] }>('checkpoint_restore', {
        turn_id: turnId,
        ...(path === undefined ? {} : { path }),
        ...(options?.confirmOlder === true ? { confirm_older: true } : {}),
      }),
    restorePreview: (turnId) => invoke<RestorePreview>('checkpoint_preview', { turn_id: turnId }),
    threadHistory: async (workspaceId, conversationId) =>
      (await invoke<{ turns: StoredTurn[] }>('thread_history', { workspace_id: workspaceId, conversation_id: conversationId })).turns,
    deleteThreadHistory: async (workspaceId, conversationId) =>
      (await invoke<{ deleted: number }>('thread_history_delete', { workspace_id: workspaceId, conversation_id: conversationId })).deleted,
    forkThreadHistory: async (request) =>
      (
        await invoke<{ copied: number }>('thread_history_fork', {
          workspace_id: request.workspaceId,
          source_conversation_id: request.sourceConversationId,
          target_conversation_id: request.targetConversationId,
          target_conversation_uuid: request.targetConversationUuid,
          ...(request.upToTurnId === undefined ? {} : { up_to_turn_id: request.upToTurnId }),
          ...(request.messageIds === undefined ? {} : { message_ids: request.messageIds }),
        })
      ).copied,
    onEvent: (handler) => listen(AGENT_EVENT_CHANNEL, (payload) => handler(payload as AgentEvent)),
  };
}

export interface TauriEventInternals {
  invoke: HostInvoke;
  transformCallback(callback: (envelope: TauriEnvelope) => void, once?: boolean): number;
}

/**
 * `listen` over `window.__TAURI_INTERNALS__` — what `@tauri-apps/api/event`
 * does, without the package (same reasoning as `hostBridge.ts`).
 */
export function tauriListen(internals: TauriEventInternals): ListenFn {
  return async (channel, handler) => {
    const callbackId = internals.transformCallback((envelope) => handler(envelope.payload));
    const eventId = await internals.invoke<number>('plugin:event|listen', {
      event: channel,
      target: { kind: 'Any' },
      handler: callbackId,
    });
    return () => {
      void internals.invoke<void>('plugin:event|unlisten', { event: channel, eventId }).catch(() => undefined);
    };
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriWorkspaceIpc(): WorkspaceIpc | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  const bound: TauriEventInternals = {
    invoke: internals.invoke.bind(internals),
    transformCallback: internals.transformCallback.bind(internals),
  };
  return createWorkspaceIpc(bound.invoke, tauriListen(bound));
}
