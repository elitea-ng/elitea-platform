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
}

export type TurnPhase = 'resolving' | 'starting' | 'running' | 'committing' | 'done' | 'cancelled' | 'error';

export interface ApprovalRequestPayload {
  request_id: string;
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

export type AgentEvent =
  | (EventBase & { kind: 'status'; payload: { phase: TurnPhase; message?: string } })
  | (EventBase & { kind: 'text_delta'; payload: { text: string } })
  | (EventBase & { kind: 'tool_call'; payload: { call_id: string; tool: string; args_summary: string; remote: boolean } })
  | (EventBase & { kind: 'tool_result'; payload: { call_id: string; ok: boolean; summary: string; truncated: boolean } })
  | (EventBase & { kind: 'approval_request'; payload: ApprovalRequestPayload })
  | (EventBase & { kind: 'error'; payload: { code: string; message: string } })
  | (EventBase & {
      kind: 'done';
      payload: { committed: boolean; conversation_id: string; message_ids: string[]; changed_files: number };
    });

export type AgentEventHandler = (event: AgentEvent) => void;

export interface WorkspaceIpc {
  open(): Promise<Workspace | null>;
  list(): Promise<Workspace[]>;
  remove(id: string): Promise<void>;
  bindProject(id: string, projectId: number): Promise<void>;
  startTurn(request: TurnStartRequest): Promise<TurnStarted>;
  cancelTurn(turnId: string): Promise<void>;
  respondApproval(requestId: string, decision: ApprovalDecision): Promise<void>;
  turnChanges(turnId: string): Promise<TurnChanges>;
  /** Restore the whole turn, or one file when `path` is given. */
  restore(turnId: string, path?: string): Promise<{ restored: string[] }>;
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

export function createWorkspaceIpc(invoke: HostInvoke, listen: ListenFn): WorkspaceIpc {
  return {
    open: () => invoke<Workspace | null>('workspace_open'),
    list: () => invoke<Workspace[]>('workspace_list'),
    remove: (id) => invoke<void>('workspace_remove', { id }),
    bindProject: (id, projectId) => invoke<void>('workspace_bind_project', { id, project_id: projectId }),
    startTurn: (request) => invoke<TurnStarted>('agent_turn_start', { ...request }),
    cancelTurn: (turnId) => invoke<void>('agent_turn_cancel', { turn_id: turnId }),
    respondApproval: (requestId, decision) => invoke<void>('approval_respond', { request_id: requestId, decision }),
    turnChanges: (turnId) => invoke<TurnChanges>('turn_changes', { turn_id: turnId }),
    restore: (turnId, path) =>
      invoke<{ restored: string[] }>('checkpoint_restore', path === undefined ? { turn_id: turnId } : { turn_id: turnId, path }),
    onEvent: (handler) => listen(AGENT_EVENT_CHANNEL, (payload) => handler(payload as AgentEvent)),
  };
}

interface TauriEventInternals {
  invoke: HostInvoke;
  transformCallback(callback: (envelope: TauriEnvelope) => void, once?: boolean): number;
}

/**
 * `listen` over `window.__TAURI_INTERNALS__` — what `@tauri-apps/api/event`
 * does, without the package (same reasoning as `hostBridge.ts`).
 */
function tauriListen(internals: TauriEventInternals): ListenFn {
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
