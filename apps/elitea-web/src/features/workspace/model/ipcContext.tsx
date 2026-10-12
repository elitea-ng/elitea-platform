/**
 * Where the workspace UI gets its host client. The default is the real Tauri
 * client (undefined in a plain browser tab); tests and Storybook provide the
 * in-memory fake instead.
 */
import { createContext, useContext, useMemo, type ReactNode } from 'react';

import { tauriWorkspaceIpc, type WorkspaceIpc } from '@/shared/desktop/workspaceIpc';

const WorkspaceIpcContext = createContext<WorkspaceIpc | undefined>(undefined);

export function WorkspaceIpcProvider({ ipc, children }: { ipc: WorkspaceIpc; children: ReactNode }): ReactNode {
  return <WorkspaceIpcContext.Provider value={ipc}>{children}</WorkspaceIpcContext.Provider>;
}

/** The provided client, else the Tauri one; `undefined` when there is no host at all. */
export function useWorkspaceIpc(): WorkspaceIpc | undefined {
  const provided = useContext(WorkspaceIpcContext);
  return useMemo(() => provided ?? tauriWorkspaceIpc(), [provided]);
}
