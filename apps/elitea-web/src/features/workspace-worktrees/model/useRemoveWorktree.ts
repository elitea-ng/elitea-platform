/**
 * Removing a thread's task worktree when the thread is archived or deleted
 * (the sidebar's Local work rows): `worktree_remove` with the PARENT folder's
 * workspace id and the worktree's thread UUID. Without `force` the host
 * refuses a worktree with uncommitted work (`worktree_dirty`): that answer is
 * `'dirty'`, for the caller to ask the person, never an error. The branch
 * stays when it has commits (the host's rule). The folder lists and the
 * folder's tasks are read again after a removal.
 */
import { useQueryClient } from '@tanstack/react-query';

import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';

import { describeWorktreeError } from './describeWorktreeError';
import { useWorktreeIpc } from './ipcContext';
import { listKey } from './useWorktrees';

/** The sidebar's and pages' folder list (`WORKSPACE_LIST_KEY`): a task worktree is a workspace too. */
const WORKSPACES_KEY = ['workspace', 'list'] as const;

/** A task worktree to remove: its parent folder's workspace id and its thread UUID. */
export interface WorktreeRef {
  parentId: string;
  threadUuid: string;
  branch: string;
}

/** A removal the host refused for a reason the person reads (already described). */
export class RemoveWorktreeError extends Error {}

export type RemoveWorktree = (worktree: WorktreeRef, force: boolean) => Promise<'removed' | 'dirty'>;

/** `undefined` outside the desktop host. */
export function useRemoveWorktree(): RemoveWorktree | undefined {
  const ipc = useWorktreeIpc();
  const queryClient = useQueryClient();
  if (ipc === undefined) return undefined;
  return async ({ parentId, threadUuid }, force) => {
    try {
      await ipc.remove(parentId, threadUuid, force);
    } catch (failure) {
      if (!force && toWorkspaceIpcError(failure).code === 'worktree_dirty') return 'dirty';
      throw new RemoveWorktreeError(describeWorktreeError(failure));
    } finally {
      await Promise.all([queryClient.invalidateQueries({ queryKey: WORKSPACES_KEY }), queryClient.invalidateQueries({ queryKey: listKey(parentId) })]);
    }
    return 'removed';
  };
}
