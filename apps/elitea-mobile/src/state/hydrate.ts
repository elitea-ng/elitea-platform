import { AppState, type AppStateStatus } from 'react-native';

import { devSignInEnabled } from '@/auth/devSignIn';
import { restoreNativeSession } from '@/auth/nativeSession';
import { restorePolicy } from '@/security/policyStore';
import { log } from '@/lib/log';
import { parseConnectLink } from '@/linking/parseConnectLink';
import { WipeIncompleteError, resumePendingWipe } from '@/security/endSession';
import { checkFreshInstall } from '@/security/freshInstall';
import { SECURE_KEYS, deleteSecureItem, getSecureItem } from '@/security/secureStorage';
import { restoreSelfIdentity } from '@/state/selfIdentity';
import { getSession, setCredential, setSession } from '@/state/session';
import { restoreBrandTheme } from '@/theme/brandStore';

/**
 * Launch-time session restore; routing (and the splash screen) waits for
 * `session.hydrated`.
 *
 * Order:
 *   1. fresh-install check: the iOS Keychain outlives an uninstall, so a
 *      reinstall starts signed out (nothing is read);
 *   2. resume an interrupted wipe (`az.wipe.pending`). If it still cannot
 *      delete every secure item, NOTHING is restored and launch finishes
 *      signed out (the marker stays, so the next launch wipes again). Waiting
 *      for it instead would keep a permanently failing key on the splash
 *      screen forever;
 *   3. read the workspace origin, the credential and the selected project.
 *
 * Secure items are `WHEN_UNLOCKED_THIS_DEVICE_ONLY`: reading them fails while
 * the device is locked. A read ERROR is therefore "try again when the app is
 * active", never "no credential" (that would look like a logout and could
 * throw away a working session). Only a definite `null` means absent.
 */

export interface HydrateOptions {
  devSignIn?: boolean;
  appState?: {
    addEventListener(type: 'change', listener: (state: AppStateStatus) => void): { remove(): void };
  };
  allowPrivateHttp?: boolean;
}

let started: Promise<void> | null = null;

export function hydrateSession(options: HydrateOptions = {}): Promise<void> {
  if (started) return started;
  started = (async () => {
    for (;;) {
      try {
        await hydrateOnce(options);
        break;
      } catch (error) {
        log.warn('secure storage unavailable at launch; retrying when the app is active', { error });
        await nextActive(options.appState ?? AppState);
      }
    }
    if (!getSession().hydrated) setSession({ hydrated: true });
  })();
  return started;
}

function nextActive(appState: NonNullable<HydrateOptions['appState']>): Promise<void> {
  return new Promise((resolve) => {
    const sub = appState.addEventListener('change', (state) => {
      if (state === 'active') {
        sub.remove();
        resolve();
      }
    });
  });
}

/** One restore attempt. Throws only when secure storage cannot be read. */
async function hydrateOnce(options: HydrateOptions): Promise<void> {
  const { fresh } = await checkFreshInstall();
  if (fresh) return;

  try {
    await resumePendingWipe();
  } catch (error) {
    if (!(error instanceof WipeIncompleteError)) throw error;
    log.warn('interrupted wipe still incomplete; starting signed out, nothing restored', { error });
    return;
  }

  const storedOrigin = await getSecureItem(SECURE_KEYS.serverOrigin);
  if (storedOrigin === null) return;
  const parsed = parseConnectLink(storedOrigin, {
    connectSchemes: [], // a stored origin is a bare URL, never a connect link
    allowPrivateHttp: options.allowPrivateHttp ?? __DEV__,
  });
  if (!parsed.ok || parsed.origin !== storedOrigin) {
    // Not something connect() wrote (or plain http stored by a dev build):
    // start from the connect screen.
    log.warn('stored workspace origin rejected at launch');
    await deleteSecureItem(SECURE_KEYS.serverOrigin);
    return;
  }

  const kind = await getSecureItem(SECURE_KEYS.credentialKind);
  const projectRaw = await getSecureItem(SECURE_KEYS.projectSelected);
  const projectId = projectRaw !== null && /^[1-9]\d{0,15}$/.test(projectRaw) ? Number(projectRaw) : null;

  // The workspace's last server policy decides the launch lock (refreshed by
  // the next token response), and its last brand avoids an unbranded flash.
  await restorePolicy(parsed.origin);
  await restoreBrandTheme(parsed.origin);
  // "Me" for shared conversations, so a launch offline still knows it.
  await restoreSelfIdentity(parsed.origin);
  setSession({ serverOrigin: parsed.origin, projectId: Number.isSafeInteger(projectId) ? projectId : null });

  if (kind === 'oauth') {
    if (await restoreNativeSession(parsed.origin)) return;
  } else if (kind === 'pat' && (options.devSignIn ?? devSignInEnabled())) {
    const token = await getSecureItem(SECURE_KEYS.pat);
    if (token !== null && token.trim() !== '') {
      setCredential({ kind: 'pat', token });
      return;
    }
  }
  if (kind !== null) {
    // A credential this build cannot use (a PAT in a build without developer
    // sign-in) or one that is unreadable or for another origin: stay
    // connected and sign in again. Nothing server-side is touched; an
    // abandoned native device session expires on the server.
    log.info('stored credential not usable by this build', { kind });
    await deleteSecureItem(SECURE_KEYS.pat);
    await deleteSecureItem(SECURE_KEYS.refreshToken);
    await deleteSecureItem(SECURE_KEYS.accessToken);
    await deleteSecureItem(SECURE_KEYS.credentialKind);
  }
}

/** Test helper. */
export function __resetHydrateForTests(): void {
  started = null;
}
