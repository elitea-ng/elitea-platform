/**
 * Where the browser pane gets its host client: the real Tauri client by
 * default (`undefined` in a plain browser tab), a fake in tests.
 */
import { createContext, useContext, useMemo, type ReactNode } from 'react';

import { tauriBrowserIpc, type BrowserIpc } from '@/shared/desktop/browserIpc';
import type { HostBridge } from '@/shared/desktop/hostBridge';
import { createHostBridge, tauriInvoke } from '@/shared/desktop/hostBridge';

interface BrowserHost {
  readonly ipc: BrowserIpc | undefined;
  /** Hands a URL to the system browser (the opener plugin). */
  readonly openExternal: HostBridge['openExternal'] | undefined;
}

const BrowserIpcContext = createContext<BrowserHost | undefined>(undefined);

export function BrowserIpcProvider({ ipc, openExternal, children }: { ipc: BrowserIpc; openExternal: HostBridge['openExternal']; children: ReactNode }): ReactNode {
  const value = useMemo(() => ({ ipc, openExternal }), [ipc, openExternal]);
  return <BrowserIpcContext.Provider value={value}>{children}</BrowserIpcContext.Provider>;
}

export function useBrowserHost(): BrowserHost {
  const provided = useContext(BrowserIpcContext);
  return useMemo(() => {
    if (provided !== undefined) return provided;
    const invoke = tauriInvoke();
    const bridge = invoke === undefined ? undefined : createHostBridge(invoke);
    return { ipc: tauriBrowserIpc(), openExternal: bridge === undefined ? undefined : (url: string) => bridge.openExternal(url) };
  }, [provided]);
}
