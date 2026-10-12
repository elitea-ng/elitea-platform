/**
 * Where the index UI gets its host client: the real Tauri one by default
 * (undefined in a plain browser tab, where the index UI renders nothing);
 * tests and the dev harness provide the in-memory fake instead.
 */
import { createContext, useContext, useMemo, type ReactNode } from 'react';

import { tauriIndexIpc, type IndexIpc } from '@/shared/desktop/indexIpc';

const IndexIpcContext = createContext<IndexIpc | undefined>(undefined);

export function IndexIpcProvider({ ipc, children }: { ipc: IndexIpc; children: ReactNode }): ReactNode {
  return <IndexIpcContext.Provider value={ipc}>{children}</IndexIpcContext.Provider>;
}

/** The provided client, else the Tauri one; `undefined` when there is no host at all. */
export function useIndexIpc(): IndexIpc | undefined {
  const provided = useContext(IndexIpcContext);
  return useMemo(() => provided ?? tauriIndexIpc(), [provided]);
}
