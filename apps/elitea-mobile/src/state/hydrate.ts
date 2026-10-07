import { AppState, type AppStateStatus } from 'react-native';

import { devSignInEnabled } from '@/auth/devSignIn';
import { restoreNativeSession } from '@/auth/nativeSession';
import { restorePolicy } from '@/security/policyStore';
import { log } from '@/lib/log';
import { parseConnectLink } from '@/linking/parseConnectLink';
import { clearAgentCatalogAtLaunch } from '@/features/intents/agentCatalog';
import { clearQuickActionsAtLaunch } from '@/features/quickActions/quickActions';
import { clearSurfaceSnapshotAtLaunch } from '@/features/surfaces/surfaceWriter';
import { resumePendingWipes, wipeStoredWorkspace } from '@/security/endSession';
import { checkFreshInstall } from '@/security/freshInstall';
import { SECURE_KEYS, bindWorkspace, deleteSecureItem, getSecureItem } from '@/security/secureStorage';
import { migrateLegacyLayout, readRegistry } from '@/security/workspaceRegistry';
import { restoreSelfIdentity } from '@/state/selfIdentity';
import { getSession, setCredential, setSession } from '@/state/session';
import { refreshWorkspaceList } from '@/state/workspaceList';
import { restoreBrandTheme } from '@/theme/brandStore';

/**
 * Launch-time session restore; routing (and the splash screen) waits for
 * `session.hydrated`.
 *
 * Order:
 *   1. fresh-install check: the iOS Keychain outlives an uninstall, so a
 *      reinstall starts signed out (nothing is read);
 *   2. move the single-workspace layout of an older app version into the
 *      first workspace slot (workspaceRegistry.ts; no data loss, no new
 *      sign-in);
 *   3. resume every interrupted wipe (a workspace's `az.wipe.pending`). A
 *      workspace whose wipe still cannot delete every secure item is NOT
 *      restored (the marker stays, so the next launch wipes again). Waiting
 *      for it instead would keep a permanently failing key on the splash
 *      screen forever;
 *   4. bind the registry's active workspace and read its origin, credential
 *      and selected project (`restoreWorkspace`, also used by a switch);
 *   5. no workspace signed in: clear the home-screen quick actions the OS
 *      kept from the previous run (a wipe resumed after a kill).
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
    // The OS kept the previous run's quick actions; only the signed-in layout
    // rebuilds them, so a launch that restores no signed-in workspace (a
    // resumed wipe, a workspace waiting for sign-in) clears them (QA-SEC-1).
    // The widget snapshot (App Group) and the Siri agent catalog outlive the
    // process the same way.
    if (getSession().credential === null) {
      void clearQuickActionsAtLaunch();
      clearSurfaceSnapshotAtLaunch();
      void clearAgentCatalogAtLaunch();
    }
    // The switcher's list (hosts, brand marks); never blocks the launch.
    void refreshWorkspaceList();
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

  await migrateLegacyLayout();
  const active = (await readRegistry()).active;
  const resumed = await resumePendingWipes();
  const resumedActive = resumed.find((r) => r.workspaceId === active);
  if (resumedActive) {
    // The active workspace was being wiped: start on the connect screen, with
    // the reason it ended (its other workspaces stay listed there).
    setSession({ endReason: resumedActive.reason });
    if (!resumedActive.wiped) log.warn('interrupted wipe still incomplete; that workspace is not restored');
    return;
  }
  if (active === null) return;
  await restoreWorkspace(active, options);
}

/**
 * Binds `workspaceId` and restores it into the session: origin, project,
 * policy, brand, "me", then the credential. Throws only when secure storage
 * cannot be read (the caller retries). A slot whose origin is missing or not
 * something `connect` wrote is wiped and nothing is restored.
 */
export async function restoreWorkspace(
  workspaceId: string,
  options: Pick<HydrateOptions, 'devSignIn' | 'allowPrivateHttp'> = {},
): Promise<boolean> {
  bindWorkspace(workspaceId);
  const storedOrigin = await getSecureItem(SECURE_KEYS.serverOrigin);
  const parsed =
    storedOrigin === null
      ? null
      : parseConnectLink(storedOrigin, {
          connectSchemes: [], // a stored origin is a bare URL, never a connect link
          allowPrivateHttp: options.allowPrivateHttp ?? __DEV__,
        });
  if (parsed === null || !parsed.ok || parsed.origin !== storedOrigin) {
    // Not something connect() wrote (or plain http stored by a dev build):
    // drop the slot and start from the connect screen.
    log.warn('stored workspace origin rejected at launch');
    bindWorkspace(null);
    await wipeStoredWorkspace(workspaceId, { reason: 'reset' });
    return false;
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
  setSession({
    serverOrigin: parsed.origin,
    workspaceId,
    projectId: Number.isSafeInteger(projectId) ? projectId : null,
  });

  if (kind === 'oauth') {
    if (await restoreNativeSession(parsed.origin)) return true;
  } else if (kind === 'pat' && (options.devSignIn ?? devSignInEnabled())) {
    const token = await getSecureItem(SECURE_KEYS.pat);
    if (token !== null && token.trim() !== '') {
      setCredential({ kind: 'pat', token });
      return true;
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
  return true;
}

/** Test helper. */
export function __resetHydrateForTests(): void {
  started = null;
}
