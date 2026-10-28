/**
 * Where the parallel-task UI gets its host client: the provided one (tests
 * provide the in-memory fake), else the real Tauri client (`undefined` in a
 * plain browser tab).
 */
import { createContext, useContext, useMemo, type ReactNode } from 'react';

import { tauriWorktreeIpc, type WorktreeIpc } from '@/shared/desktop/worktreeIpc';

const WorktreeIpcContext = createContext<WorktreeIpc | undefined>(undefined);

export function WorktreeIpcProvider({ ipc, children }: { ipc: WorktreeIpc; children: ReactNode }): ReactNode {
  return <WorktreeIpcContext.Provider value={ipc}>{children}</WorktreeIpcContext.Provider>;
}

/** The provided client, else the Tauri one; `undefined` when there is no host at all. */
export function useWorktreeIpc(): WorktreeIpc | undefined {
  const provided = useContext(WorktreeIpcContext);
  return useMemo(() => provided ?? tauriWorktreeIpc(), [provided]);
}
