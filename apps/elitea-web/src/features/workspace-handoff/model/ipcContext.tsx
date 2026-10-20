/**
 * Where the handoff UI gets its host client: the real Tauri one by default
 * (undefined in a plain browser tab, where it renders nothing); tests and the
 * dev harness provide the in-memory fake instead.
 */
import { createContext, useContext, useMemo, type ReactNode } from 'react';

import { tauriHandoffIpc, type HandoffIpc } from '@/shared/desktop/handoffIpc';

const HandoffIpcContext = createContext<HandoffIpc | undefined>(undefined);

export function HandoffIpcProvider({ ipc, children }: { ipc: HandoffIpc; children: ReactNode }): ReactNode {
  return <HandoffIpcContext.Provider value={ipc}>{children}</HandoffIpcContext.Provider>;
}

/** The provided client, else the Tauri one; `undefined` when there is no host at all. */
export function useHandoffIpc(): HandoffIpc | undefined {
  const provided = useContext(HandoffIpcContext);
  return useMemo(() => provided ?? tauriHandoffIpc(), [provided]);
}
