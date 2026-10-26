/**
 * A folder's task worktrees and what the panel does to them: the list
 * (`worktree_list`), "New parallel task" (`worktree_create` for a fresh
 * thread UUID) and remove. Remove asks the host first without `force`; a
 * `worktree_dirty` answer (uncommitted work) becomes `confirming`, and only
 * the person's confirmation retries with `force`. Every change re-reads the
 * list, so the counts are the host's.
 */
import { useState } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import type { WorktreeEntry, WorktreeIpc } from '@/shared/desktop/worktreeIpc';

import { describeWorktreeError } from './describeWorktreeError';

const listKey = (workspaceId: string) => ['worktree', 'list', workspaceId] as const;

export function useWorktrees(ipc: WorktreeIpc, workspaceId: string) {
  const queryClient = useQueryClient();
  const list = useQuery({ queryKey: listKey(workspaceId), queryFn: () => ipc.list(workspaceId) });
  const refresh = (): Promise<void> => queryClient.invalidateQueries({ queryKey: listKey(workspaceId) });
  const [error, setError] = useState<string | null>(null);
  const [confirming, setConfirming] = useState<WorktreeEntry | null>(null);

  const create = useMutation({
    mutationFn: () => ipc.create(workspaceId, crypto.randomUUID()),
    onMutate: () => setError(null),
    onError: (failure) => setError(describeWorktreeError(failure)),
    onSettled: refresh,
  });
  const remove = useMutation({
    mutationFn: ({ entry, force }: { entry: WorktreeEntry; force: boolean }) => ipc.remove(workspaceId, entry.thread_uuid, force),
    onMutate: () => setError(null),
    onSuccess: () => setConfirming(null),
    onError: (failure, { entry, force }) => {
      // A forced remove's failure is shown in the confirmation, which stays open.
      if (force) return;
      if (toWorkspaceIpcError(failure).code === 'worktree_dirty') setConfirming(entry);
      else setError(describeWorktreeError(failure));
    },
    onSettled: refresh,
  });
  const forcedFailure = remove.isError && remove.variables.force ? describeWorktreeError(remove.error) : null;

  return {
    entries: list.data ?? [],
    refresh,
    error,
    clearError: () => setError(null),
    create: () => create.mutate(),
    creating: create.isPending,
    remove: (entry: WorktreeEntry) => remove.mutate({ entry, force: false }),
    removing: remove.isPending,
    /** The task whose uncommitted work waits for the person's confirmation. */
    confirming,
    confirmRemove: () => confirming !== null && remove.mutate({ entry: confirming, force: true }),
    cancelRemove: () => {
      remove.reset();
      setConfirming(null);
    },
    forcedFailure,
  };
}
