import type { ReactNode } from 'react';

import { log } from '@/lib/log';
import { parseConnectLink, type ParseConnectLinkOptions } from '@/linking/parseConnectLink';
import { endSession } from '@/security/endSession';
import { SECURE_KEYS, setSecureItem } from '@/security/secureStorage';
import { getSession, setSession, useSession } from '@/state/session';

/**
 * Workspace (server) selection: a thin adapter over the session store in
 * `session.ts`, kept so the screens written against the original context API
 * (`serverOrigin`, `connect`, `clear`) need no change.
 */
interface WorkspaceState {
  serverOrigin: string | null;
  /**
   * Selects the workspace server and persists it (secure storage) BEFORE the
   * session changes, so the next launch restores it behind the splash screen.
   * Resolves false when it could not be stored. Signing in is a separate step.
   */
  connect: (origin: string) => Promise<boolean>;
  /** Logs out and wipes everything stored for the workspace (`endSession`). */
  clear: () => Promise<void>;
}

/** Kept for source compatibility; the session store needs no provider. */
export function WorkspaceProvider({ children }: { children: ReactNode }) {
  return children;
}

export async function connectWorkspace(origin: string): Promise<boolean> {
  try {
    await setSecureItem(SECURE_KEYS.serverOrigin, origin);
  } catch (error) {
    log.warn('could not store the workspace origin', { error });
    return false;
  }
  setSession({ serverOrigin: origin, endReason: null, lastOriginHint: null, pendingConnectOrigin: null });
  return true;
}

/**
 * Switches to another workspace: everything stored for the current one is
 * wiped first (`endSession({reason:'server_switch'})`). The new origin is NOT
 * connected here: it is handed to the connect screen
 * (`pendingConnectOrigin`), which reads its discovery document and asks the
 * user to confirm the host like any other address. The caller must have
 * asked the user to confirm the wipe. Returns false when the wipe could not
 * delete every secure item.
 */
export async function switchWorkspace(origin: string): Promise<boolean> {
  if (getSession().serverOrigin === origin) return true;
  const { storageWiped } = await endSession({ reason: 'server_switch' });
  // A credential of the old workspace may still be stored: offering the new
  // origin now would pair it with that origin at the next launch. The kept
  // wipe marker makes the next launch finish the wipe first.
  if (!storageWiped) {
    log.warn('workspace switch stopped: the old workspace could not be fully wiped');
    return false;
  }
  setSession({ pendingConnectOrigin: origin });
  return true;
}

const clear = async () => {
  await endSession({ reason: 'user_logout' });
};

export function useWorkspace(): WorkspaceState {
  const serverOrigin = useSession((s) => s.serverOrigin);
  return { serverOrigin, connect: connectWorkspace, clear };
}

/** What to do with a URL the OS opened the app with. */
export type IncomingLinkAction =
  /** Not a connect link (OAuth redirect, dev-client URL, …), or unusable. */
  | { kind: 'ignore' }
  /** Not connected: the connect screen handles it (it reads the `server` param). */
  | { kind: 'connect'; origin: string }
  /** The workspace already in use: nothing to do. */
  | { kind: 'same' }
  /** Another workspace while connected: ask, then `switchWorkspace(origin)`. */
  | { kind: 'switch'; from: string; origin: string };

export function classifyIncomingLink(
  url: string | null,
  currentOrigin: string | null,
  options: ParseConnectLinkOptions,
): IncomingLinkAction {
  if (!url) return { kind: 'ignore' };
  const isConnectLink = options.connectSchemes.some((scheme) =>
    url.toLowerCase().startsWith(`${scheme.toLowerCase()}://connect`),
  );
  if (!isConnectLink) return { kind: 'ignore' };
  const parsed = parseConnectLink(url, options);
  if (!parsed.ok) return { kind: 'ignore' };
  if (currentOrigin === null) return { kind: 'connect', origin: parsed.origin };
  if (currentOrigin === parsed.origin) return { kind: 'same' };
  return { kind: 'switch', from: currentOrigin, origin: parsed.origin };
}
