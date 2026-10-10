/**
 * One workspace's local index: its status (`index_status`, kept current by
 * `index://event`), the progress of a running build, and the actions the
 * settings dialog offers. A policy that turns the index off is a state
 * (`disabled`), not an error: the UI shows why instead of controls.
 *
 * `index_status` never opens an index: the workspace list shows every
 * folder's chip at launch without opening or refreshing any. With `open`
 * (the folder's session page), the index is opened instead (`index_open`),
 * which also checks the folder for what changed while it was closed.
 */
import { useEffect, useMemo, useState } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { OFF_STATUS, type IndexIpc, type IndexStatus } from '@/shared/desktop/indexIpc';
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';

import { foldProgress, type IndexProgress } from './progress';

export type IndexView = { kind: 'status'; status: IndexStatus } | { kind: 'disabled' };

/** `refresh` reads only what changed; `rebuild` reads everything again. */
export type IndexAction = 'enable' | 'refresh' | 'rebuild' | 'cancel' | 'disable' | 'remove';

export interface WorkspaceIndex {
  /** `undefined` while the first read is pending or after it failed (`loadError`). */
  view: IndexView | undefined;
  loadError: unknown;
  progress: IndexProgress | null;
  run: (action: IndexAction) => void;
  pending: boolean;
  actionError: unknown;
}

const indexQueryKey = (workspaceId: string, open: boolean) => ['workspace', 'index', workspaceId, open ? 'open' : 'status'] as const;

const DISABLED: IndexView = { kind: 'disabled' };

const statusView = (status: IndexStatus): IndexView => ({ kind: 'status', status });

function isPolicyOff(error: unknown): boolean {
  return toWorkspaceIpcError(error).code === 'local_index_disabled';
}

async function readIndex(ipc: IndexIpc, workspaceId: string, open: boolean): Promise<IndexView> {
  try {
    return statusView(await (open ? ipc.open(workspaceId) : ipc.status(workspaceId)));
  } catch (error) {
    if (isPolicyOff(error)) return DISABLED;
    throw error;
  }
}

/** Runs one action; the status it leaves, or `null` when only a re-read knows. */
async function perform(ipc: IndexIpc, workspaceId: string, action: IndexAction): Promise<IndexStatus | null> {
  switch (action) {
    case 'enable':
      return ipc.enable(workspaceId);
    case 'refresh':
      return ipc.refresh(workspaceId, false);
    case 'rebuild':
      return ipc.refresh(workspaceId, true);
    case 'cancel':
      await ipc.cancel(workspaceId);
      return null;
    case 'disable':
      return ipc.disable(workspaceId);
    case 'remove':
      await ipc.remove(workspaceId);
      return OFF_STATUS;
  }
}

export interface UseWorkspaceIndexOptions {
  /** Open the index (`index_open`) rather than only read its status: the folder's session page. */
  open?: boolean;
}

export function useWorkspaceIndex(ipc: IndexIpc, workspaceId: string, options: UseWorkspaceIndexOptions = {}): WorkspaceIndex {
  const open = options.open === true;
  const queryClient = useQueryClient();
  const key = useMemo(() => indexQueryKey(workspaceId, open), [workspaceId, open]);
  const query = useQuery({ queryKey: key, queryFn: () => readIndex(ipc, workspaceId, open) });
  const [progress, setProgress] = useState<IndexProgress | null>(null);

  useEffect(() => {
    let unlisten: (() => void) | undefined;
    let disposed = false;
    void ipc
      .onEvent((event) => {
        if (event.workspace_id !== workspaceId) return;
        queryClient.setQueryData<IndexView>(key, statusView(event.status));
        setProgress((previous) => foldProgress(previous, event));
      })
      .then((off) => {
        if (disposed) off();
        else unlisten = off;
      })
      .catch(() => undefined);
    return () => {
      disposed = true;
      unlisten?.();
    };
  }, [ipc, workspaceId, key, queryClient]);

  const action = useMutation({
    mutationFn: (next: IndexAction) => perform(ipc, workspaceId, next),
    onSuccess: (status, next) => {
      if (next === 'remove' || next === 'disable') setProgress(null);
      if (status === null) void queryClient.invalidateQueries({ queryKey: key });
      else queryClient.setQueryData<IndexView>(key, statusView(status));
    },
    onError: (error) => {
      // The policy changed under the open dialog: show why, not the controls.
      if (isPolicyOff(error)) queryClient.setQueryData<IndexView>(key, DISABLED);
    },
  });

  return {
    view: query.data,
    loadError: query.error,
    progress: query.data?.kind === 'status' && query.data.status.state === 'building' ? progress : null,
    run: action.mutate,
    pending: action.isPending,
    actionError: action.error !== null && !isPolicyOff(action.error) ? action.error : null,
  };
}
