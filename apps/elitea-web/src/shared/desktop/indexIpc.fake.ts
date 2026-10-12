/**
 * An in-memory `IndexIpc` for tests and the dev harness: the host's rules
 * (policy gate, `index_off`, cancel answering whether a refresh ran) over a
 * map of statuses. `emit` plays the host's side of `index://event`; `set`
 * puts an index in a given state without one.
 */
import type { IndexEvent, IndexEventHandler, IndexIpc, IndexStatus } from './indexIpc';
import { OFF_STATUS } from './indexIpc';
import { WorkspaceIpcError } from './workspaceIpc';

type FailableCommand = 'status' | 'open' | 'enable' | 'disable' | 'refresh' | 'cancel' | 'remove';

export interface FakeIndexIpc extends IndexIpc {
  /** Deliver an event to every subscriber (and record its status). */
  emit(event: IndexEvent): void;
  /** What `index_status` answers for a workspace from now on. */
  set(workspaceId: string, status: Partial<IndexStatus>): void;
  /** Turn the policy gate on or off (`local_index_disabled`). */
  setPolicyAllowed(allowed: boolean): void;
  /** Make the next call of `command` reject the way the host does. */
  failNext(command: FailableCommand, code: string, message: string): void;
  subscriberCount(): number;
  readonly calls: { opened: string[]; enabled: string[]; disabled: string[]; refreshes: { workspaceId: string; full: boolean }[]; cancelled: string[]; removed: string[] };
}

export interface FakeIndexOptions {
  statuses?: Record<string, Partial<IndexStatus>>;
  /** `false`: the policy turns the index off. Default `true`. */
  policyAllowed?: boolean;
}

export function createFakeIndexIpc(options: FakeIndexOptions = {}): FakeIndexIpc {
  const statuses = new Map<string, IndexStatus>(
    Object.entries(options.statuses ?? {}).map(([id, status]) => [id, { ...OFF_STATUS, on_disk: status.state !== undefined && status.state !== 'off', ...status }]),
  );
  const handlers = new Set<IndexEventHandler>();
  const failures = new Map<FailableCommand, WorkspaceIpcError>();
  const calls: FakeIndexIpc['calls'] = { opened: [], enabled: [], disabled: [], refreshes: [], cancelled: [], removed: [] };
  let policyAllowed = options.policyAllowed ?? true;

  const current = (id: string): IndexStatus => statuses.get(id) ?? { ...OFF_STATUS };
  const failure = (command: FailableCommand): Promise<never> | undefined => {
    const error = failures.get(command);
    if (error === undefined) return undefined;
    failures.delete(command);
    return Promise.reject(error);
  };
  const gate = (command: FailableCommand): Promise<never> | undefined =>
    failure(command) ??
    (policyAllowed ? undefined : Promise.reject(new WorkspaceIpcError('local_index_disabled', 'The local index is turned off by your organisation’s policy.')));

  return {
    calls,
    status(workspaceId) {
      const failed = failure('status');
      if (failed !== undefined) return failed;
      // The policy off is a status, not an error: the person may still turn the index off or remove it.
      if (!policyAllowed) return Promise.resolve({ ...OFF_STATUS, policy_off: true, on_disk: current(workspaceId).on_disk });
      return Promise.resolve({ ...current(workspaceId) });
    },
    open(workspaceId) {
      calls.opened.push(workspaceId);
      const refused = gate('open');
      if (refused !== undefined) return refused;
      // Opening checks the folder: an index that is on starts a refresh.
      const status = current(workspaceId);
      if (status.state === 'stale' || status.state === 'stale_policy') statuses.set(workspaceId, { ...status, state: 'building' });
      return Promise.resolve({ ...current(workspaceId) });
    },
    enable(workspaceId) {
      calls.enabled.push(workspaceId);
      const refused = gate('enable');
      if (refused !== undefined) return refused;
      statuses.set(workspaceId, { ...current(workspaceId), state: 'building', error: null, on_disk: true });
      return Promise.resolve({ ...current(workspaceId) });
    },
    disable(workspaceId) {
      calls.disabled.push(workspaceId);
      const failed = failure('disable');
      if (failed !== undefined) return failed;
      // The build is kept.
      statuses.set(workspaceId, { ...OFF_STATUS, on_disk: current(workspaceId).on_disk });
      return Promise.resolve({ ...current(workspaceId), policy_off: !policyAllowed });
    },
    refresh(workspaceId, full) {
      calls.refreshes.push({ workspaceId, full: full === true });
      const refused = gate('refresh');
      if (refused !== undefined) return refused;
      if (current(workspaceId).state === 'off') return Promise.reject(new WorkspaceIpcError('index_off', 'Turn the index on for this workspace first.'));
      statuses.set(workspaceId, { ...current(workspaceId), state: 'building' });
      return Promise.resolve({ ...current(workspaceId) });
    },
    cancel(workspaceId) {
      calls.cancelled.push(workspaceId);
      const failed = failure('cancel');
      if (failed !== undefined) return failed;
      const status = current(workspaceId);
      if (status.state !== 'building') return Promise.resolve(false);
      statuses.set(workspaceId, { ...status, state: status.last_run === null ? 'error' : 'stale' });
      return Promise.resolve(true);
    },
    remove(workspaceId) {
      calls.removed.push(workspaceId);
      const failed = failure('remove');
      if (failed !== undefined) return failed;
      statuses.delete(workspaceId);
      return Promise.resolve();
    },
    onEvent(handler) {
      handlers.add(handler);
      return Promise.resolve(() => {
        handlers.delete(handler);
      });
    },
    emit(event) {
      statuses.set(event.workspace_id, { ...event.status });
      Array.from(handlers).forEach((handler) => handler(event));
    },
    set(workspaceId, status) {
      statuses.set(workspaceId, { ...current(workspaceId), ...status });
    },
    setPolicyAllowed(allowed) {
      policyAllowed = allowed;
    },
    failNext(command, code, message) {
      failures.set(command, new WorkspaceIpcError(code, message));
    },
    subscriberCount: () => handlers.size,
  };
}
