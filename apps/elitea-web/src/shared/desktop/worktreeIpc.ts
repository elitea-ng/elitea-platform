/**
 * The typed client for the desktop host's task worktrees (IPC.md,
 * `worktree_*`; #1235). A task worktree is a git worktree
 * of a repository folder on its own `elitea/<id>` branch, so an agent can work
 * there in parallel with the main folder; its work comes back with a merge
 * (previewed first, checkpointed, undoable). The `worktree_*` commands take
 * the PARENT workspace's id; an entry's own `workspace_id` is what turns run
 * in. Wire shapes are the host's (snake_case). Failures reject with a
 * `WorkspaceIpcError` (`worktree_dirty`, `merge_conflicts`, …).
 */
import type { HostInvoke } from './hostBridge';
import { tauriInvoke } from './hostBridge';
import { toWorkspaceIpcError } from './workspaceIpc';

export interface WorktreeEntry {
  /** The worktree's own workspace: turns that run in it take this id. */
  workspace_id: string;
  thread_uuid: string;
  path: string;
  /** `elitea/<first 8 hex of the thread uuid>`. */
  branch: string;
  base_sha: string;
  /** Unix seconds. */
  created_at: number;
  /** Commits on the branch since `base_sha`. */
  ahead: number;
  /** Changed paths not committed in the worktree. */
  dirty: number;
  /** Unix seconds of its last turn; `null` before the first. */
  last_turn_at: number | null;
  /** The folder is gone (the Doctor prunes it). */
  missing: boolean;
}

/** Where a new worktree starts: the folder's HEAD, or a turn's checkpoint. */
type WorktreeBase = 'head' | `checkpoint:${number}`;

export interface WorktreeDiff {
  /** Unified diff against `base_sha`; denied paths are left out. */
  diff: string;
  truncated: boolean;
}

/**
 * The dry run of a merge back (writes nothing). In `conflicts` mode both
 * sides changed `conflict_paths` and nothing would be applied: the work stays
 * on the branch.
 */
export interface MergePreflight {
  preflight_id: string;
  mode: 'fast_forward' | 'merge' | 'conflicts' | 'nothing';
  /** What applying writes or deletes in the main folder. */
  applied_paths: string[];
  conflict_paths: string[];
  /** Never written: denied paths, symlinks, submodules, `.git`. */
  skipped_paths: string[];
  branch: string;
}

export interface MergeApplied {
  written: string[];
  deleted: string[];
  skipped_paths: string[];
  /** The main folder's checkpoint taken before applying: `undo` takes it. */
  checkpoint_id: string | null;
}

export interface WorktreeIpc {
  create(workspaceId: string, threadUuid: string, base?: WorktreeBase): Promise<WorktreeEntry>;
  /** Newest first. */
  list(workspaceId: string): Promise<WorktreeEntry[]>;
  diff(workspaceId: string, threadUuid: string): Promise<WorktreeDiff>;
  /** Rejects `worktree_dirty` for uncommitted work unless `force`. */
  remove(workspaceId: string, threadUuid: string, force: boolean): Promise<void>;
  preflight(workspaceId: string, threadUuid: string): Promise<MergePreflight>;
  apply(preflightId: string): Promise<MergeApplied>;
  undo(checkpointId: string): Promise<{ restored: string[] }>;
}

export function createWorktreeIpc(invoke: HostInvoke): WorktreeIpc {
  const call = <T>(command: string, args: Record<string, unknown>): Promise<T> =>
    invoke<T>(command, args).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  return {
    create: (workspaceId, threadUuid, base) =>
      call<WorktreeEntry>('worktree_create', base === undefined ? { workspace_id: workspaceId, thread_uuid: threadUuid } : { workspace_id: workspaceId, thread_uuid: threadUuid, base }),
    list: (workspaceId) => call<WorktreeEntry[]>('worktree_list', { workspace_id: workspaceId }),
    diff: (workspaceId, threadUuid) => call<WorktreeDiff>('worktree_diff', { workspace_id: workspaceId, thread_uuid: threadUuid }),
    remove: (workspaceId, threadUuid, force) => call<null>('worktree_remove', { workspace_id: workspaceId, thread_uuid: threadUuid, force }).then(() => undefined),
    preflight: (workspaceId, threadUuid) => call<MergePreflight>('worktree_merge_preflight', { workspace_id: workspaceId, thread_uuid: threadUuid }),
    apply: (preflightId) => call<MergeApplied>('worktree_merge_apply', { preflight_id: preflightId }),
    undo: (checkpointId) => call<{ restored: string[] }>('worktree_merge_undo', { checkpoint_id: checkpointId }),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriWorktreeIpc(): WorktreeIpc | undefined {
  const invoke = tauriInvoke();
  return invoke === undefined ? undefined : createWorktreeIpc(invoke);
}
