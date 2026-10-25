/**
 * The typed client for the desktop host's workspace + agent-turn IPC
 * (ADR-0029 D0). Commands go through the same `invoke` seam as `hostBridge`;
 * turn progress arrives on one event channel, `agent://event`.
 *
 * The wire shapes are the host's contract (snake_case, exactly as the Rust
 * side serialises them) and are NOT renamed here: a rename layer is one more
 * place for the two halves to drift apart.
 */
import type { ContextMeter } from './contextMeter';
import type { AttachedImage, CustomCommand, StoredImage, TurnStartRequest } from './turnStart';
import type { HostInvoke } from './hostBridge';
import type { PlanProposalPayload, PlanView } from './planIpc';
import type { ProcessTurnEvent } from './processTypes';
import type { SubAgentFinishedPayload, SubAgentInnerEvent, SubAgentStartedPayload } from './subAgentIpc';
import type { ThinkingDeltaPayload, ThinkingEndPayload, TurnModel } from './turnModel';
import { threadForkCommands, type ThreadForkCommands } from './threadForkIpc';
import type { UsagePayload, UsageTotalPayload } from './turnUsage';

export type { ContextMeter } from './contextMeter';
export type { AttachedImage, CustomCommand, TurnCommand, TurnImagePick, TurnMode, TurnStartRequest } from './turnStart';
export type { ThreadForkRequest } from './threadForkIpc';

export interface Workspace {
  id: string;
  path: string;
  name: string;
  project_id: number | null;
  is_git: boolean;
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

/**
 * `continue` / `cancel` answer an `mcp_authorization` request; `accept` / `decline` a `plan_proposed` card; the rest a tool's
 * approval (`continue` takes `allow_once` too). `deny_with_feedback` denies any card with the person's text (IPC.md "Decline with feedback").
 */
export type ApprovalDecision = 'allow_once' | 'allow_always' | 'deny' | 'deny_with_feedback' | 'continue' | 'cancel' | 'accept' | 'decline';
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
  /**
   * `continue`: the run used its steps; asked every time, shown as a card in the transcript.
   * `mcp_authorization`: a remote tool's MCP server needs the person's sign-in in the browser, then `continue` (or `cancel`).
   * Absent for a tool's approval.
   */
  kind?: 'continue' | 'mcp_authorization';
  /** For `continue`: the steps each run takes (and the next one would). */
  steps?: number;
  /** For `mcp_authorization`: the tool call it holds up; the card goes when that call's `tool_result` arrives. */
  call_id?: string;
  /** For `mcp_authorization`: the toolkit's name. */
  toolkit?: string;
  /** For `mcp_authorization`: the MCP server's address, to show. */
  server_url?: string;
  /** For `mcp_authorization`: the web app page to sign in on (`/app/<project>/mcps/all/<toolkit>`), relative to the deployment; absent when there is none. */
  authorize_path?: string;
  tool: string;
  title: string;
  detail: string;
  command?: string[];
  paths?: string[];
  reason: string;
  /** The layer that asks: `workspace` / `user` (a rule), `default`, … (#1216). */
  source?: string;
  /** The rule that asks (`workspace#3`, `user#1`), when a rule does. */
  rule?: string;
  /** The rule's justification, shown in place of the classifier's reason. */
  justification?: string;
  can_remember: boolean;
  /** `session`: "always" lasts until the app quits (host-intel tools, #1238). */
  remember_scope?: 'session';
}

export type { SubAgentStatus } from './subAgentIpc';

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

/** `model_retry`: the retry (from 1) of `max_attempts`, and what the failed call streamed. */
export interface ModelRetryPayload {
  attempt: number;
  max_attempts: number;
  code: string;
  message: string;
  delay_ms: number;
  text_units: number;
  thinking_units: number;
}

export type AgentEvent =
  | (EventBase & {
      kind: 'status';
      payload: {
        phase: TurnPhase;
        message?: string;
        project_instructions?: string[];
        /** On the first `running`: the model the turn runs on. */
        model?: TurnModel;
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
  | (EventBase & { kind: 'thinking_delta'; payload: ThinkingDeltaPayload })
  | (EventBase & { kind: 'thinking_end'; payload: ThinkingEndPayload })
  | (EventBase & { kind: 'tool_call'; payload: { call_id: string; tool: string; args_summary: string; remote: boolean } })
  | (EventBase & { kind: 'tool_result'; payload: { call_id: string; ok: boolean; summary: string; truncated: boolean } })
  /** One of the turn's model calls (`turnUsage.ts`): `prompt_tokens` is what it sent (the context in use); `context` its request occupancy (the composer's meter). Absent when the gateway reports no usage. */
  | (EventBase & { kind: 'usage'; payload: UsagePayload })
  | (EventBase & { kind: 'usage_total'; payload: UsageTotalPayload })
  /** The request occupancy when its phase changes (`compacting` while a summary call runs, then `compacted`, then `measured`): the composer's meter between `usage` events. */
  | (EventBase & { kind: 'context'; payload: ContextMeter })
  | (EventBase & { kind: 'approval_request'; payload: ApprovalRequestPayload })
  /** The host closed an open request nobody answered within its wait (an MCP sign-in): it no longer waits. */
  | (EventBase & { kind: 'approval_closed'; payload: { request_id: string; reason: string } })
  /** The agent's checklist changed: the whole plan, `null` once deleted. */
  | (EventBase & { kind: 'plan_updated'; payload: { plan: PlanView | null } })
  | (EventBase & { kind: 'plan_proposed'; payload: PlanProposalPayload })
  /** The person accepted the plan: the session left plan mode and the turn goes on with every tool. */
  | (EventBase & { kind: 'plan_accepted'; payload: { call_id: string } })
  /** A read-only sub-agent started under the turn's `spawn_agent` call `parent_call_id` (#1226; IPC.md "Sub-agents"). */
  | (EventBase & { kind: 'subagent_started'; payload: SubAgentStartedPayload })
  /** One of a sub-agent's own events: its `tool_call`, `tool_result` or `text_delta`, as `inner`. */
  | (EventBase & { kind: 'subagent_event'; payload: { agent_id: string; inner: SubAgentInnerEvent } })
  /** A sub-agent settled: once per run of it (a `send_input` that continues it settles again). */
  | (EventBase & { kind: 'subagent_finished'; payload: SubAgentFinishedPayload })
  /** A background process its `run_command` call started, its output and its end, while this turn runs (#1223). */
  | (EventBase & ProcessTurnEvent)
  /**
   * A model call failed in a way another call may mend (a cut or stalled stream, the gateway unreachable, a 408,
   * 409, 429 or 5xx) and is made again after `delay_ms`. `text_units` and `thinking_units` are what the failed call
   * streamed (UTF-16 code units): they come off the end of the transcript.
   */
  | (EventBase & { kind: 'model_retry'; payload: ModelRetryPayload })
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
  /** The images the turn carried: thumbnails only. Absent for a turn recorded before images. */
  images?: StoredImage[];
  /** `interrupted`: it never ended (the app quit while it ran). */
  state: 'done' | 'running' | 'interrupted';
  /** The host still keeps it: `turn_changes` / `checkpoint_restore` answer for it. */
  live: boolean;
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

export interface WorkspaceIpc extends ThreadForkCommands {
  open(): Promise<Workspace | null>;
  list(): Promise<Workspace[]>;
  remove(id: string): Promise<void>;
  bindProject(id: string, projectId: number): Promise<void>;
  /** The "@" picker: the workspace's files and folders matching `query`, best first. */
  files(workspaceId: string, query: string, limit?: number): Promise<WorkspaceFile[]>;
  /** The "/" menu's custom commands of the workspace and of the person's own folder, by name. */
  commands(workspaceId: string): Promise<CustomCommand[]>;
  startTurn(request: TurnStartRequest): Promise<TurnStarted>;
  /** Attach the image file at `path` (absolute: a drop's, as `files_dropped` reports it) to the workspace's next turn; the host reads it confined and shrinks it. */
  attachFile(workspaceId: string, path: string): Promise<AttachedImage>;
  /** Attach a pasted image (`data`: base64, at most 10 MiB decoded). */
  attachBytes(workspaceId: string, mime: string, data: string, name?: string): Promise<AttachedImage>;
  /** Take an attached image off before it is sent. */
  removeAttachment(workspaceId: string, token: string): Promise<void>;
  /** Send a turn's failed commit again (after `commit_failed`); resolves once it is saved. */
  retryCommit(executionId: string): Promise<void>;
  /** Have the host read ahead what a turn to this agent reads first (the composer got focus); starts nothing. */
  prewarm(request: TurnPrewarmRequest): Promise<TurnPrewarmed>;
  cancelTurn(turnId: string): Promise<void>;
  /** Where a turn is, for a UI that may have missed its `done` event (events are not replayed). */
  turnStatus(turnId: string): Promise<TurnStatus>;
  /** `feedback` (the person's text for the agent) goes only with `decline` / `deny_with_feedback`. */
  respondApproval(requestId: string, decision: ApprovalDecision, feedback?: string): Promise<void>;
  turnChanges(turnId: string): Promise<TurnChanges>;
  /** Undo the newest turn (or one file of a turn), or with `confirmOlder` restore the folder to before an older turn. */
  restore(turnId: string, path?: string, options?: RestoreOptions): Promise<{ restored: string[] }>;
  /** What restoring the folder to before the turn would write back and delete now; changes nothing. */
  restorePreview(turnId: string): Promise<RestorePreview>;
  /** The turns this machine recorded in a thread, oldest first; `[]` when it recorded none. */
  threadHistory(workspaceId: string, conversationId: string): Promise<StoredTurn[]>;
  /** Forget a thread's recorded turns (the conversation itself stays); resolves with how many. */
  deleteThreadHistory(workspaceId: string, conversationId: string): Promise<number>;
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
    commands: (workspaceId) => invoke<CustomCommand[]>('commands_list', { workspace_id: workspaceId }),
    startTurn: (request) => invoke<TurnStarted>('agent_turn_start', { ...request }),
    attachFile: (workspaceId, path) => invoke<AttachedImage>('attachment_add', { workspace_id: workspaceId, path }),
    attachBytes: (workspaceId, mime, data, name) =>
      invoke<AttachedImage>('attachment_add_bytes', { workspace_id: workspaceId, mime, data, ...(name === undefined ? {} : { name }) }),
    removeAttachment: (workspaceId, token) => invoke<void>('attachment_remove', { workspace_id: workspaceId, token }),
    prewarm: (request) => invoke<TurnPrewarmed>('agent_prewarm', { ...request }),
    retryCommit: (executionId) => invoke<void>('turn_commit_retry', { execution_id: executionId }),
    cancelTurn: (turnId) => invoke<void>('agent_turn_cancel', { turn_id: turnId }),
    turnStatus: (turnId) => invoke<TurnStatus>('agent_turn_status', { turn_id: turnId }),
    respondApproval: (requestId, decision, feedback) =>
      invoke<void>('approval_respond', feedback === undefined ? { request_id: requestId, decision } : { request_id: requestId, decision, feedback }),
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
    ...threadForkCommands(invoke),
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
