/**
 * "Work in › New worktree" (a new thread's composer): a task worktree of the
 * folder for a fresh thread UUID, on its own `elitea/<id>` branch from the
 * folder's HEAD. The first message then goes to the worktree's own workspace
 * (`entry.workspace_id`). The folder lists and the folder's tasks are read
 * again, so the sidebar shows the task under its folder at once.
 */
import { useQueryClient } from '@tanstack/react-query';

import { tauriWorktreeIpc, type WorktreeEntry, type WorktreeIpc } from '@/shared/desktop/worktreeIpc';

import { describeWorktreeError } from './describeWorktreeError';
import { listKey } from './useWorktrees';

/** The sidebar's and pages' folder list (`WORKSPACE_LIST_KEY`): a task worktree is a workspace too. */
const WORKSPACES_KEY = ['workspace', 'list'] as const;

export class StartInWorktreeError extends Error {}

export function useStartInWorktree(provided?: WorktreeIpc): ((workspaceId: string) => Promise<WorktreeEntry>) | undefined {
  const queryClient = useQueryClient();
  const ipc = provided ?? tauriWorktreeIpc();
  if (ipc === undefined) return undefined;
  return async (workspaceId) => {
    let entry: WorktreeEntry;
    try {
      entry = await ipc.create(workspaceId, crypto.randomUUID());
    } catch (failure) {
      throw new StartInWorktreeError(describeWorktreeError(failure));
    }
    await Promise.all([queryClient.invalidateQueries({ queryKey: WORKSPACES_KEY }), queryClient.invalidateQueries({ queryKey: listKey(workspaceId) })]);
    return entry;
  };
}
