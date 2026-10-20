import { AppState, type AppStateStatus } from 'react-native';

import { log } from '@/lib/log';
import { network } from '@/lib/network';
import { getSession } from '@/state/session';
import { refreshWorkspaceInfo } from '@/state/workspaceInfo';

/**
 * Re-reads the connected workspace's discovery document whenever the app
 * comes back from the background (and, below, when the network comes back) (DEF-R1, DEF-R4). It carries the public
 * client policy and the minimum app version, so:
 *  - a workspace that starts requiring a device lock locks a running app on
 *    its next foreground instead of after the next access-token refresh and a
 *    relaunch;
 *  - a lowered minimum version clears the "Update Agent Zefir" screen.
 *
 * Only a real return from 'background' counts: iOS passes through 'inactive'
 * for Control Center and the Face ID sheet, which must not refetch. Uses the
 * OS AppState, not appActivity, so a locked app (which appActivity reports as
 * background) still learns about a changed policy.
 */

interface AppStateLike {
  currentState?: string | null;
  addEventListener(type: 'change', listener: (state: AppStateStatus) => void): { remove(): void };
}

export interface ForegroundRefreshDeps {
  appState: AppStateLike;
  refresh: () => Promise<unknown>;
  isConnected: () => boolean;
}

export function bindForegroundRefresh(deps: ForegroundRefreshDeps): () => void {
  let wasBackground = deps.appState.currentState === 'background';
  const sub = deps.appState.addEventListener('change', (state) => {
    if (state === 'background') {
      wasBackground = true;
      return;
    }
    if (state !== 'active' || !wasBackground) return;
    wasBackground = false;
    if (!deps.isConnected()) return;
    deps.refresh().catch((error: unknown) => log.info('discovery refresh on foreground failed', { error }));
  });
  return () => sub.remove();
}

export interface ReconnectRefreshDeps {
  network: { subscribe(listener: (online: boolean) => void): () => void };
  refresh: () => Promise<unknown>;
  isConnected: () => boolean;
}

/**
 * Re-reads discovery when the device comes back online (F1). The document is
 * kept in memory only, so a launch without network leaves it unknown: the
 * contract-gated features (Settings > Usage and Memory, swipe-to-dismiss)
 * would stay off until the next foreground or token refresh.
 */
export function bindReconnectRefresh(deps: ReconnectRefreshDeps): () => void {
  return deps.network.subscribe((online) => {
    if (!online || !deps.isConnected()) return;
    deps.refresh().catch((error: unknown) => log.info('discovery refresh on reconnect failed', { error }));
  });
}

let started = false;

/** Starts the app's binding (idempotent). */
export function startForegroundRefresh(): void {
  if (started) return;
  started = true;
  bindForegroundRefresh({
    appState: AppState,
    refresh: () => refreshWorkspaceInfo(),
    isConnected: () => getSession().serverOrigin !== null,
  });
  bindReconnectRefresh({
    network,
    refresh: () => refreshWorkspaceInfo(),
    isConnected: () => getSession().serverOrigin !== null,
  });
}
