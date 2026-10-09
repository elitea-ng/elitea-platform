/**
 * An in-memory `WorkspaceIpc` for tests and Storybook: the same surface, no
 * host. `emit` plays the host's side of the event channel, so a test can
 * script a turn (including out-of-order and duplicate events) exactly.
 */
import type {
  AgentEvent,
  AgentEventHandler,
  ApprovalDecision,
  TurnChanges,
  TurnStartRequest,
  TurnStarted,
  Workspace,
  WorkspaceIpc,
} from './workspaceIpc';

export interface FakeWorkspaceIpc extends WorkspaceIpc {
  /** Deliver an event to every subscriber. */
  emit(event: AgentEvent): void;
  readonly calls: {
    started: TurnStartRequest[];
    cancelled: string[];
    approvals: { requestId: string; decision: ApprovalDecision }[];
    restores: { turnId: string; path?: string }[];
  };
  setChanges(turnId: string, changes: TurnChanges): void;
  subscriberCount(): number;
}

export interface FakeOptions {
  workspaces?: Workspace[];
  /** What `workspace_open` hands back (the folder the user "picked"). */
  nextOpen?: Workspace | null;
}

export function createFakeWorkspaceIpc(options: FakeOptions = {}): FakeWorkspaceIpc {
  let workspaces = [...(options.workspaces ?? [])];
  const handlers = new Set<AgentEventHandler>();
  const changes = new Map<string, TurnChanges>();
  const calls: FakeWorkspaceIpc['calls'] = { started: [], cancelled: [], approvals: [], restores: [] };
  let turnCounter = 0;

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
      workspaces = workspaces.filter((w) => w.id !== id);
      return Promise.resolve();
    },
    bindProject(id, projectId) {
      workspaces = workspaces.map((w) => (w.id === id ? { ...w, project_id: projectId } : w));
      return Promise.resolve();
    },
    startTurn(request): Promise<TurnStarted> {
      calls.started.push(request);
      turnCounter += 1;
      return Promise.resolve({ turn_id: `turn-${String(turnCounter)}`, execution_id: `exec-${String(turnCounter)}` });
    },
    cancelTurn(turnId) {
      calls.cancelled.push(turnId);
      return Promise.resolve();
    },
    respondApproval(requestId, decision) {
      calls.approvals.push({ requestId, decision });
      return Promise.resolve();
    },
    turnChanges(turnId) {
      return Promise.resolve(changes.get(turnId) ?? { files: [] });
    },
    restore(turnId, path) {
      calls.restores.push(path === undefined ? { turnId } : { turnId, path });
      const files = changes.get(turnId)?.files ?? [];
      return Promise.resolve({ restored: path === undefined ? files.map((f) => f.path) : [path] });
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
    subscriberCount: () => handlers.size,
  };
}
