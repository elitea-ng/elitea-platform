/**
 * What the account menu shows about this install: the deployment's display
 * name (the tenant the person is connected to) and the app version, read
 * once from the host (`host_state`). Both are absent outside the desktop app
 * (the dev harness, a browser tab).
 */
import { useEffect, useState } from 'react';

import { tauriInvoke, type HostState } from '@/shared/desktop/hostBridge';

export interface HostInfo {
  readonly tenant: string | undefined;
  readonly version: string | undefined;
}

const NONE: HostInfo = { tenant: undefined, version: undefined };

export function useHostInfo(): HostInfo {
  const [info, setInfo] = useState<HostInfo>(NONE);
  useEffect(() => {
    const invoke = tauriInvoke();
    if (invoke === undefined) return undefined;
    let live = true;
    invoke<HostState>('host_state')
      .then((state) => {
        if (live) setInfo({ tenant: state.displayName ?? undefined, version: state.clientVersion });
      })
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, []);
  return info;
}
