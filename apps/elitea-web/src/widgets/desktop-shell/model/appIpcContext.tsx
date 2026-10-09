/**
 * Where the shell gets the host's native-shell client (`app_platform`,
 * reveal/open, `app://command`). Default: the real Tauri client, `undefined`
 * in a plain browser tab. Tests and the dev harness provide the fake.
 */
import { createContext, useContext, useMemo, type ReactNode } from 'react';

import { tauriAppIpc, type AppIpc } from '@/shared/desktop/appEvents';

const AppIpcContext = createContext<AppIpc | undefined>(undefined);

export function AppIpcProvider({ ipc, children }: { ipc: AppIpc; children: ReactNode }): ReactNode {
  return <AppIpcContext.Provider value={ipc}>{children}</AppIpcContext.Provider>;
}

export function useAppIpc(): AppIpc | undefined {
  const provided = useContext(AppIpcContext);
  return useMemo(() => provided ?? tauriAppIpc(), [provided]);
}
