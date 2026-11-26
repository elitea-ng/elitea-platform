/**
 * A folder's trust state, read from the host (`trust_status`) and refreshed
 * when the host says it changed (`trust://event`: a grant, a revoke, a
 * suspension by the policy or a file changed outside the app). The grant
 * and revoke write the answer straight into the cache, then refresh every
 * folder: a worktree follows its parent, and a tamper pauses them all.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { createContext, createElement, useContext, useEffect, useMemo, type ReactNode } from 'react';

import { tauriTrustIpc, type TrustIpc, type TrustStatus } from '@/shared/desktop/trustIpc';

import { TRUST_CONFIRMATION_VERSION } from './trustCopy';

const TrustIpcContext = createContext<TrustIpc | undefined>(undefined);

/** Tests and the desktop harness hand in their own client. */
export function TrustIpcProvider({ ipc, children }: { ipc: TrustIpc; children: ReactNode }): React.JSX.Element {
  return createElement(TrustIpcContext.Provider, { value: ipc }, children);
}

/** The trust client, or `undefined` outside the desktop host. */
export function useTrustIpc(): TrustIpc | undefined {
  const provided = useContext(TrustIpcContext);
  return useMemo(() => provided ?? tauriTrustIpc(), [provided]);
}

const TRUST_ROOT = ['folder-trust'] as const;
const statusKey = (workspaceId: string) => [...TRUST_ROOT, 'status', workspaceId] as const;
export const TRUST_LIST_KEY = [...TRUST_ROOT, 'list'] as const;

/** Refetch every trust read whenever the host sends `trust://event`. */
export function useTrustEvents(ipc: TrustIpc | undefined): void {
  const queryClient = useQueryClient();
  useEffect(() => {
    if (ipc === undefined) return undefined;
    let unlisten: (() => void) | undefined;
    let gone = false;
    void ipc
      .onEvent(() => void queryClient.invalidateQueries({ queryKey: TRUST_ROOT }))
      .then((stop) => (gone ? stop() : (unlisten = stop)))
      .catch(() => undefined);
    return () => {
      gone = true;
      unlisten?.();
    };
  }, [ipc, queryClient]);
}

export interface FolderTrust {
  ipc: TrustIpc | undefined;
  /** `undefined` while loading, outside the desktop host, or when the host cannot say. */
  status: TrustStatus | undefined;
}

export function useFolderTrust(workspaceId: string | undefined): FolderTrust {
  const ipc = useTrustIpc();
  useTrustEvents(ipc);
  const query = useQuery({
    queryKey: statusKey(workspaceId ?? ''),
    queryFn: () => (ipc as TrustIpc).status(workspaceId as string),
    enabled: ipc !== undefined && workspaceId !== undefined && workspaceId !== '',
    retry: false,
  });
  return { ipc, status: query.data };
}

/** Grant and revoke; each answer replaces the folder's cached state. */
export function useTrustActions(ipc: TrustIpc | undefined) {
  const queryClient = useQueryClient();
  const settle = (status: TrustStatus): void => {
    queryClient.setQueryData(statusKey(status.workspace_id), status);
    void queryClient.invalidateQueries({ queryKey: TRUST_ROOT });
  };
  const grant = useMutation({
    mutationFn: ({ workspaceId, askEveryCommand }: { workspaceId: string; askEveryCommand: boolean }) =>
      (ipc as TrustIpc).grant(workspaceId, TRUST_CONFIRMATION_VERSION, askEveryCommand),
    onSuccess: settle,
  });
  const revoke = useMutation({ mutationFn: (workspaceId: string) => (ipc as TrustIpc).revoke(workspaceId), onSuccess: settle });
  return { grant, revoke };
}

/** The folder whose trust a status reads: a worktree's parent, else the folder itself. */
export const trustOwner = (status: Pick<TrustStatus, 'workspace_id' | 'inherited_from'>): string => status.inherited_from ?? status.workspace_id;
