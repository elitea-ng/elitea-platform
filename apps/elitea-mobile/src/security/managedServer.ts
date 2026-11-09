import { useMemo } from 'react';

import { identity } from '@/config/identity';
import { log } from '@/lib/log';
import { parseConnectLink } from '@/linking/parseConnectLink';
import { useManagedConfig, type ManagedConfig } from '@/security/managedConfig';

/**
 * The workspace an MDM / EMM assigns (`server_url`, ADR-0052 W1b): its
 * canonical origin, as the connect screen would store it. A `server_url` that
 * is not a workspace address assigns nothing (it is logged, never shown as
 * a half-trusted value).
 */
export function managedServerOrigin(managed: ManagedConfig, allowPrivateHttp: boolean = __DEV__): string | null {
  if (managed.serverUrl === null) return null;
  const parsed = parseConnectLink(managed.serverUrl, { connectSchemes: identity.connectSchemes, allowPrivateHttp });
  if (!parsed.ok) {
    log.warn('managed server_url ignored: not a workspace address', { error: parsed.error });
    return null;
  }
  return parsed.origin;
}

/** `lock_server`: the origin the device keeps ONLY, or null when nothing is locked. */
export function lockedOrigin(managed: ManagedConfig, allowPrivateHttp: boolean = __DEV__): string | null {
  return managed.lockServer === true ? managedServerOrigin(managed, allowPrivateHttp) : null;
}

export interface ManagedServer {
  /** The assigned workspace; the connect screen shows it read only and offers no other. */
  origin: string | null;
  /** `lock_server` too: no workspace can be added. */
  locked: boolean;
}

export function useManagedServer(): ManagedServer {
  const managed = useManagedConfig();
  return useMemo(() => {
    const origin = managedServerOrigin(managed);
    return { origin, locked: origin !== null && managed.lockServer === true };
  }, [managed]);
}
