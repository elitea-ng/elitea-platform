/**
 * What the account menu shows about this install: the deployment's display
 * name (the tenant the person is connected to), its origin (where the admin
 * portal lives), the app version and the IT support line, read once from the
 * host (`host_state`).
 * All are absent outside the desktop app (the dev harness, a browser tab).
 */
import { useEffect, useState } from 'react';

import { effectiveSupportContact } from '@elitea/client-core/support';

import { tauriInvoke, type HostState } from '@/shared/desktop/hostBridge';

export interface HostInfo {
  readonly tenant: string | undefined;
  readonly version: string | undefined;
  readonly origin: string | undefined;
  /** The IT support line (ADR-0052 W6.2): the managed one, else the workspace policy's. */
  readonly supportContact: string | undefined;
}

const NONE: HostInfo = { tenant: undefined, version: undefined, origin: undefined, supportContact: undefined };

/** The support line of a host state: managed beats the stored `client_policy.support_contact`. */
export function hostSupportContact(state: Pick<HostState, 'policy' | 'managedSupportContact'>): string | undefined {
  return effectiveSupportContact(state.managedSupportContact, state.policy?.['support_contact']) ?? undefined;
}

export function useHostInfo(): HostInfo {
  const [info, setInfo] = useState<HostInfo>(NONE);
  useEffect(() => {
    const invoke = tauriInvoke();
    if (invoke === undefined) return undefined;
    let live = true;
    invoke<HostState>('host_state')
      .then((state) => {
        if (live) {
          setInfo({
            tenant: state.displayName ?? undefined,
            version: state.clientVersion,
            origin: state.origin ?? undefined,
            supportContact: hostSupportContact(state),
          });
        }
      })
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, []);
  return info;
}
