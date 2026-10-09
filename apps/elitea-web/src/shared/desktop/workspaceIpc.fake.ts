/**
 * An in-memory `WorkspaceIpc` for tests and Storybook: the same surface, no
 * host. `emit` plays the host's side of the event channel, so a test can
 * script a turn (including out-of-order and duplicate events) exactly.
 */
import type {
  AgentEvent,
  AgentEventHandler,
  ApprovalDecision,
  StoredTurn,
  TurnChanges,
  TurnStartRequest,
  TurnStarted,
  TurnStatus,
  Workspace,
  WorkspaceFile,
  WorkspaceIpc,
} from './workspaceIpc';
import { WorkspaceIpcError } from './workspaceIpc';

type FailableCommand =
  | 'remove'
  | 'bindProject'
  | 'files'
  | 'startTurn'
  | 'cancelTurn'
  | 'turnStatus'
  | 'respondApproval'
  | 'turnChanges'
  | 'restore'
  | 'threadHistory'
  | 'deleteThreadHistory';

export interface FakeWorkspaceIpc extends WorkspaceIpc {
  /** Deliver an event to every subscriber. */
  emit(event: AgentEvent): void;
  readonly calls: {
    started: TurnStartRequest[];
    cancelled: string[];
    approvals: { requestId: string; decision: ApprovalDecision }[];
    restores: { turnId: string; path?: string }[];
    fileQueries: { workspaceId: string; query: string; limit?: number }[];
  };
  setChanges(turnId: string, changes: TurnChanges): void;
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
}

export function createFakeWorkspaceIpc(options: FakeOptions = {}): FakeWorkspaceIpc {
  let workspaces = [...(options.workspaces ?? [])];
  const handlers = new Set<AgentEventHandler>();
  const changes = new Map<string, TurnChanges>();
  const statuses = new Map<string, TurnStatus>();
  const history = new Map<string, StoredTurn[]>();
  const historyKey = (workspaceId: string, conversationId: string): string => `${workspaceId}\u0000${conversationId}`;
  const calls: FakeWorkspaceIpc['calls'] = { started: [], cancelled: [], approvals: [], restores: [], fileQueries: [] };
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
    respondApproval(requestId, decision) {
      calls.approvals.push({ requestId, decision });
      const failed = failure('respondApproval');
      if (failed !== undefined) return failed;
      return Promise.resolve();
    },
    turnChanges(turnId) {
      const failed = failure('turnChanges');
      if (failed !== undefined) return failed;
      return Promise.resolve(changes.get(turnId) ?? { files: [] });
    },
    restore(turnId, path) {
      calls.restores.push(path === undefined ? { turnId } : { turnId, path });
      const failed = failure('restore');
      if (failed !== undefined) return failed;
      const files = changes.get(turnId)?.files ?? [];
      return Promise.resolve({ restored: path === undefined ? files.map((f) => f.path) : [path] });
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
      changes.set(turnId, value);
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
