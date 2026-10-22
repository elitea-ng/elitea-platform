/**
 * An in-memory `WorkspaceIpc` for tests and Storybook: the same surface, no
 * host. `emit` plays the host's side of the event channel, so a test can
 * script a turn (including out-of-order and duplicate events) exactly.
 */
import type {
  AgentEvent,
  AgentEventHandler,
  ApprovalDecision,
  RestorePreview,
  StoredTurn,
  ThreadForkRequest,
  TurnChanges,
  TurnPrewarmRequest,
  TurnStartRequest,
  TurnStarted,
  TurnStatus,
  CustomCommand,
  Workspace,
  WorkspaceFile,
  WorkspaceIpc,
} from './workspaceIpc';
import { WorkspaceIpcError } from './workspaceIpc';

type FailableCommand =
  | 'remove'
  | 'bindProject'
  | 'files'
  | 'commands'
  | 'startTurn'
  | 'cancelTurn'
  | 'turnStatus'
  | 'respondApproval'
  | 'turnChanges'
  | 'restore'
  | 'restorePreview'
  | 'threadHistory'
  | 'deleteThreadHistory'
  | 'forkThreadHistory'
  | 'retryCommit';

export interface FakeWorkspaceIpc extends WorkspaceIpc {
  /** Deliver an event to every subscriber. */
  emit(event: AgentEvent): void;
  readonly calls: {
    started: TurnStartRequest[];
    prewarmed: TurnPrewarmRequest[];
    commitRetries: string[];
    cancelled: string[];
    approvals: { requestId: string; decision: ApprovalDecision; feedback?: string | undefined }[];
    restores: { turnId: string; path?: string; confirmOlder?: boolean }[];
    fileQueries: { workspaceId: string; query: string; limit?: number }[];
    forks: ThreadForkRequest[];
  };
  /** What `turn_changes` answers; `latest` defaults to true, `undone` to false. */
  setChanges(turnId: string, changes: Pick<TurnChanges, 'files'> & Partial<TurnChanges>): void;
  /** What `checkpoint_preview` answers (default: every changed file restored). */
  setPreview(turnId: string, preview: RestorePreview): void;
  /** What `thread_history` answers for one thread (default: nothing recorded). */
  setHistory(workspaceId: string, conversationId: string, turns: StoredTurn[]): void;
  /** What `agent_turn_status` answers for `turnId` (default: running). */
  setTurnStatus(turnId: string, status: TurnStatus): void;
  /** Make the next call of `command` reject the way the host does: `{code, message}` as a `WorkspaceIpcError`. */
  failNext(command: FailableCommand, code: string, message: string): void;
  subscriberCount(): number;
}

export interface FakeOptions {
  workspaces?: Workspace[];
  /** What `workspace_open` hands back (the folder the user "picked"). */
  nextOpen?: Workspace | null;
  /** Every workspace's files for `workspace_files`; the fake answers the ones whose path contains the query. */
  files?: WorkspaceFile[];
  /** What `commands_list` answers for every workspace (default: none). */
  commands?: CustomCommand[];
}

/** What the host refuses a restore with, given the turn's offer. */
function refusal(current: TurnChanges | undefined, path: string | undefined, confirmOlder: boolean): Promise<never> | undefined {
  if (current?.undone === true) return Promise.reject(new WorkspaceIpcError('already_undone', 'This turn’s changes were already undone.'));
  // "Undo" is for the newest turn; an older one needs the confirmation.
  if (path === undefined && current?.latest === false && !confirmOlder) {
    return Promise.reject(new WorkspaceIpcError('undo_not_latest', 'Only the newest turn of a folder can be undone.'));
  }
  return undefined;
}

export function createFakeWorkspaceIpc(options: FakeOptions = {}): FakeWorkspaceIpc {
  let workspaces = [...(options.workspaces ?? [])];
  const handlers = new Set<AgentEventHandler>();
  const changes = new Map<string, TurnChanges>();
  const previews = new Map<string, RestorePreview>();
  const statuses = new Map<string, TurnStatus>();
  const history = new Map<string, StoredTurn[]>();
  const historyKey = (workspaceId: string, conversationId: string): string => `${workspaceId}\u0000${conversationId}`;
  const calls: FakeWorkspaceIpc['calls'] = { started: [], prewarmed: [], commitRetries: [], cancelled: [], approvals: [], restores: [], fileQueries: [], forks: [] };
  let turnCounter = 0;
  const failures = new Map<FailableCommand, WorkspaceIpcError>();
  const failure = (command: FailableCommand): Promise<never> | undefined => {
    const error = failures.get(command);
    if (error === undefined) return undefined;
    failures.delete(command);
    return Promise.reject(error);
  };

  return {
    calls,
    open() {
      const picked = options.nextOpen ?? null;
      if (picked !== null && !workspaces.some((w) => w.id === picked.id)) workspaces = [...workspaces, picked];
      return Promise.resolve(picked);
    },
    list() {
      return Promise.resolve([...workspaces]);
    },
    remove(id) {
      const failed = failure('remove');
      if (failed !== undefined) return failed;
      workspaces = workspaces.filter((w) => w.id !== id);
      for (const key of history.keys()) if (key.startsWith(`${id}\u0000`)) history.delete(key);
      return Promise.resolve();
    },
    bindProject(id, projectId) {
      const failed = failure('bindProject');
      if (failed !== undefined) return failed;
      workspaces = workspaces.map((w) => (w.id === id ? { ...w, project_id: projectId } : w));
      return Promise.resolve();
    },
    files(workspaceId, query, limit) {
      calls.fileQueries.push(limit === undefined ? { workspaceId, query } : { workspaceId, query, limit });
      const failed = failure('files');
      if (failed !== undefined) return failed;
      const needle = query.toLowerCase();
      const found = (options.files ?? []).filter((file) => file.path.toLowerCase().includes(needle));
      return Promise.resolve(found.slice(0, limit ?? 50));
    },
    commands() {
      return failure('commands') ?? Promise.resolve([...(options.commands ?? [])]);
    },
    retryCommit(executionId) {
      calls.commitRetries.push(executionId);
      return failure('retryCommit') ?? Promise.resolve();
    },
    prewarm(request) {
      calls.prewarmed.push(request);
      return Promise.resolve({ version: true, participant: request.conversation_id !== undefined });
    },
    startTurn(request): Promise<TurnStarted> {
      calls.started.push(request);
      const failed = failure('startTurn');
      if (failed !== undefined) return failed;
      turnCounter += 1;
      return Promise.resolve({ turn_id: `turn-${String(turnCounter)}`, execution_id: `exec-${String(turnCounter)}` });
    },
    cancelTurn(turnId) {
      calls.cancelled.push(turnId);
      const failed = failure('cancelTurn');
      if (failed !== undefined) return failed;
      return Promise.resolve();
    },
    turnStatus(turnId) {
      const failed = failure('turnStatus');
      if (failed !== undefined) return failed;
      return Promise.resolve(statuses.get(turnId) ?? { state: 'running', done: null });
    },
    respondApproval(requestId, decision, feedback) {
      calls.approvals.push(feedback === undefined ? { requestId, decision } : { requestId, decision, feedback });
      const failed = failure('respondApproval');
      if (failed !== undefined) return failed;
      return Promise.resolve();
    },
    turnChanges(turnId) {
      const failed = failure('turnChanges');
      if (failed !== undefined) return failed;
      return Promise.resolve(changes.get(turnId) ?? { files: [], latest: true, undone: false });
    },
    restore(turnId, path, options) {
      const confirmOlder = options?.confirmOlder === true;
      calls.restores.push({ turnId, ...(path === undefined ? {} : { path }), ...(confirmOlder ? { confirmOlder } : {}) });
      const refused = failure('restore') ?? refusal(changes.get(turnId), path, confirmOlder);
      if (refused !== undefined) return refused;
      const current = changes.get(turnId);
      if (path === undefined && current !== undefined) changes.set(turnId, { ...current, latest: false, undone: true });
      return Promise.resolve({ restored: path === undefined ? (current?.files ?? []).map((f) => f.path) : [path] });
    },
    restorePreview(turnId) {
      const failed = failure('restorePreview');
      if (failed !== undefined) return failed;
      const files = changes.get(turnId)?.files ?? [];
      return Promise.resolve(previews.get(turnId) ?? { restored: files.map((f) => f.path), deleted: [] });
    },
    threadHistory(workspaceId, conversationId) {
      const failed = failure('threadHistory');
      if (failed !== undefined) return failed;
      return Promise.resolve(history.get(historyKey(workspaceId, conversationId)) ?? []);
    },
    deleteThreadHistory(workspaceId, conversationId) {
      const failed = failure('deleteThreadHistory');
      if (failed !== undefined) return failed;
      const key = historyKey(workspaceId, conversationId);
      const deleted = history.get(key)?.length ?? 0;
      history.delete(key);
      return Promise.resolve(deleted);
    },
    forkThreadHistory(request) {
      calls.forks.push(request);
      const failed = failure('forkThreadHistory');
      if (failed !== undefined) return failed;
      const source = history.get(historyKey(request.workspaceId, request.sourceConversationId)) ?? [];
      const end = request.upToTurnId === undefined ? source.length : source.findIndex((turn) => turn.turn_id === request.upToTurnId) + 1;
      if (end === 0 && request.upToTurnId !== undefined) return Promise.reject(new WorkspaceIpcError('turn_unknown', 'That turn is not in this thread.'));
      const renamed = (ids: readonly string[]): string[] => ids.map((id) => request.messageIds?.[id] ?? id);
      const copied = source.slice(0, end).map((turn, index) => ({
        ...turn,
        // As the host does: a copied `done` names the fork's messages.
        events: turn.events.map((event) =>
          event.kind === 'done' ? { ...event, payload: { ...event.payload, message_ids: renamed(event.payload.message_ids) } } : event,
        ),
        turn_id: `${turn.turn_id}-fork-${String(index)}`,
        conversation_id: request.targetConversationId,
        conversation_uuid: request.targetConversationUuid,
      }));
      if (copied.length > 0) history.set(historyKey(request.workspaceId, request.targetConversationId), copied);
      return Promise.resolve(copied.length);
    },
    onEvent(handler) {
      handlers.add(handler);
      return Promise.resolve(() => {
        handlers.delete(handler);
      });
    },
    emit(event) {
      // Copy first: a handler may unsubscribe while we iterate.
      Array.from(handlers).forEach((handler) => handler(event));
    },
    setChanges(turnId, value) {
      changes.set(turnId, { latest: true, undone: false, ...value });
    },
    setPreview(turnId, preview) {
      previews.set(turnId, preview);
    },
    setHistory(workspaceId, conversationId, turns) {
      history.set(historyKey(workspaceId, conversationId), turns);
    },
    setTurnStatus(turnId, status) {
      statuses.set(turnId, status);
    },
    failNext(command, code, message) {
      failures.set(command, new WorkspaceIpcError(code, message));
    },
    subscriberCount: () => handlers.size,
  };
}
