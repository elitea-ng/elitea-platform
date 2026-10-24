/**
 * An in-memory `WorktreeIpc` for tests: the folder's worktrees, a scripted
 * diff and merge preview, and every call recorded. It models what the host
 * decides: `remove` of a worktree with uncommitted work rejects
 * `worktree_dirty` unless forced, and `apply` of a conflicts preview rejects
 * `merge_conflicts`. `failNext` makes one command reject with a host code.
 */
import type { MergeApplied, MergePreflight, WorktreeDiff, WorktreeEntry, WorktreeIpc } from './worktreeIpc';
import { WorkspaceIpcError } from './workspaceIpc';

type Command = keyof WorktreeIpc;

export interface FakeWorktreeIpc extends WorktreeIpc {
  readonly calls: { [K in Command]: unknown[][] };
  failNext(command: Command, code: string, message?: string): void;
}

export interface FakeWorktreeScript {
  entries?: WorktreeEntry[];
  diff?: WorktreeDiff;
  preflight?: Omit<MergePreflight, 'preflight_id'>;
  applied?: MergeApplied;
}

export function fakeWorktreeEntry(overrides: Partial<WorktreeEntry> = {}): WorktreeEntry {
  return {
    workspace_id: 'wt-1',
    thread_uuid: '0b2c4d6e-0000-4000-8000-000000000001',
    path: '/tmp/repo-worktrees/0b2c4d6e',
    branch: 'elitea/0b2c4d6e',
    base_sha: 'abc123',
    created_at: 1_760_000_000,
    ahead: 0,
    dirty: 0,
    last_turn_at: null,
    missing: false,
    ...overrides,
  };
}

export function createFakeWorktreeIpc(script: FakeWorktreeScript = {}): FakeWorktreeIpc {
  let entries = [...(script.entries ?? [])];
  const calls: FakeWorktreeIpc['calls'] = { create: [], list: [], diff: [], remove: [], preflight: [], apply: [], undo: [] };
  const failures = new Map<Command, WorkspaceIpcError>();
  let previews = 0;
  let lastPreview: MergePreflight | undefined;

  const run = <T>(command: Command, args: unknown[], answer: () => T): Promise<T> => {
    calls[command].push(args);
    const failure = failures.get(command);
    if (failure !== undefined) {
      failures.delete(command);
      return Promise.reject(failure);
    }
    try {
      return Promise.resolve(answer());
    } catch (error) {
      return Promise.reject(error instanceof Error ? error : new Error(String(error)));
    }
  };

  return {
    calls,
    failNext(command, code, message = code) {
      failures.set(command, new WorkspaceIpcError(code, message));
    },
    create: (workspaceId, threadUuid, base) =>
      run('create', [workspaceId, threadUuid, base], () => {
        const entry = fakeWorktreeEntry({ workspace_id: `wt-${threadUuid}`, thread_uuid: threadUuid, branch: `elitea/${threadUuid.replaceAll('-', '').slice(0, 8)}` });
        entries = [entry, ...entries];
        return entry;
      }),
    list: (workspaceId) => run('list', [workspaceId], () => entries.map((entry) => ({ ...entry }))),
    diff: (workspaceId, threadUuid) => run('diff', [workspaceId, threadUuid], () => script.diff ?? { diff: '', truncated: false }),
    remove: (workspaceId, threadUuid, force) =>
      run('remove', [workspaceId, threadUuid, force], () => {
        const entry = entries.find((e) => e.thread_uuid === threadUuid);
        if (entry === undefined) throw new WorkspaceIpcError('worktree_unknown', 'no such worktree');
        if (entry.dirty > 0 && !force) throw new WorkspaceIpcError('worktree_dirty', 'the worktree has uncommitted changes');
        entries = entries.filter((e) => e !== entry);
      }),
    preflight: (workspaceId, threadUuid) =>
      run('preflight', [workspaceId, threadUuid], () => {
        previews += 1;
        const base = script.preflight ?? { mode: 'nothing' as const, applied_paths: [], conflict_paths: [], skipped_paths: [], branch: 'elitea/0b2c4d6e' };
        lastPreview = { ...base, preflight_id: `pf-${String(previews)}` };
        return lastPreview;
      }),
    apply: (preflightId) =>
      run('apply', [preflightId], () => {
        if (lastPreview?.preflight_id !== preflightId) throw new WorkspaceIpcError('preflight_expired', 'preview again');
        if (lastPreview.mode === 'conflicts') throw new WorkspaceIpcError('merge_conflicts', 'both sides changed these files');
        return script.applied ?? { written: [...lastPreview.applied_paths], deleted: [], skipped_paths: [...lastPreview.skipped_paths], checkpoint_id: 'cp-1' };
      }),
    undo: (checkpointId) => run('undo', [checkpointId], () => ({ restored: [...(lastPreview?.applied_paths ?? [])] })),
  };
}
