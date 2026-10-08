import { AppState } from 'react-native';

import { checkInStoredSession, type StoredCheckIn } from '@/auth/nativeSession';
import { offlineStore } from '@/db/offlineStore';
import { log } from '@/lib/log';
import { policyFrom } from '@/security/policyStore';
import { SECURE_KEYS, boundWorkspace, getSecureItemIn, setSecureItemIn } from '@/security/secureStorage';
import { readRegistry } from '@/security/workspaceRegistry';
import { bindForegroundRefresh } from '@/state/foregroundRefresh';
import { getSession, subscribeSession } from '@/state/session';
import { wipeRevokedWorkspace } from '@/state/workspace';

/**
 * Workspaces that are put aside still hear from their servers (WS-SEC-1).
 *
 * Only the active workspace sends requests, so without this a workspace the
 * user is not working in never learnt that its admin revoked this device,
 * deactivated the app or turned offline storage off: its refresh token and
 * encrypted cache stayed until it was opened again. At launch and when the
 * app returns to the foreground, every connected workspace that is not active
 * and signed in natively is checked in, at most once per
 * `CHECK_IN_INTERVAL_MS`:
 *
 *   - its device session is refreshed (`checkInStoredSession`; the rotated
 *     pair goes into its own slot);
 *   - revoked, expired, deactivated (`device_revoked`), or a refused refresh
 *     token or client (`rejected`): the workspace is wiped, as the active one
 *     would be;
 *   - the token response's client policy is stored as the workspace's policy,
 *     and a policy without offline storage deletes its encrypted database at
 *     once (the active workspace's binding does the same);
 *   - a policy that keeps offline data for `offline_retention_days` > 0
 *     deletes the database when the workspace was not used for longer than
 *     that (`offlineStore.expireIfUnused`, #45): its key is behind the device
 *     lock, so retention cannot run inside it until it is opened, and a file
 *     not open for N days holds nothing an N-day retention would keep.
 *
 * A developer sign-in (PAT) has no device session to check, and a server that
 * does not answer sends no policy: the age check then uses the slot's stored
 * policy (the last one its server sent). One workspace at a time; failures
 * change nothing and are retried at the next interval.
 */

export const CHECK_IN_INTERVAL_MS = 6 * 60 * 60 * 1000;

export interface CheckInDeps {
  now: () => number;
  checkIn: (workspaceId: string) => Promise<StoredCheckIn>;
  wipe: (workspaceId: string, reason: 'device_revoked' | 'credential_rejected') => Promise<unknown>;
  /** Deletes the workspace's encrypted database file and key. */
  wipeOfflineStore: (workspaceId: string) => Promise<void>;
  /** Deletes them when the workspace was not used for longer than `retentionDays`; true when it did. */
  expireOfflineStore: (workspaceId: string, retentionDays: number) => Promise<boolean>;
}

const defaultDeps: CheckInDeps = {
  now: Date.now,
  checkIn: checkInStoredSession,
  wipe: wipeRevokedWorkspace,
  wipeOfflineStore: (workspaceId) => offlineStore.wipe(workspaceId),
  expireOfflineStore: (workspaceId, retentionDays) => offlineStore.expireIfUnused(workspaceId, retentionDays),
};

const lastCheckIn = new Map<string, number>();
let running: Promise<void> | null = null;

function isActive(workspaceId: string): boolean {
  return boundWorkspace() === workspaceId || getSession().workspaceId === workspaceId;
}

/** Checks in every workspace that is put aside and due (coalesced). Never throws. */
export function checkInPutAsideWorkspaces(deps: CheckInDeps = defaultDeps): Promise<void> {
  if (running) return running;
  running = (async () => {
    let ids: string[];
    try {
      ids = (await readRegistry()).ids;
    } catch (error) {
      log.info('workspace check-in skipped: registry unreadable', { error });
      return;
    }
    for (const id of ids) {
      if (isActive(id)) continue;
      const now = deps.now();
      const last = lastCheckIn.get(id);
      if (last !== undefined && now - last < CHECK_IN_INTERVAL_MS) continue;
      lastCheckIn.set(id, now);
      try {
        await checkInOne(id, deps);
      } catch (error) {
        log.info('workspace check-in failed', { error });
      }
    }
  })().finally(() => {
    running = null;
  });
  return running;
}

async function checkInOne(id: string, deps: CheckInDeps): Promise<void> {
  // A wipe under way is finished when the workspace is opened or at launch.
  if ((await getSecureItemIn(id, SECURE_KEYS.wipePending)) !== null) return;
  const result = await deps.checkIn(id);
  switch (result.kind) {
    case 'device_revoked':
      log.warn('a workspace put aside was revoked by its server; wiping it');
      await deps.wipe(id, 'device_revoked');
      return;
    case 'rejected':
      log.warn('a workspace put aside refused its sign-in; wiping it');
      await deps.wipe(id, 'credential_rejected');
      return;
    case 'ok': {
      if (result.policy === undefined) break;
      if (isActive(id)) return;
      if ((await getSecureItemIn(id, SECURE_KEYS.wipePending)) !== null) return;
      await setSecureItemIn(id, SECURE_KEYS.policy, JSON.stringify({ v: 1, origin: result.origin, policy: result.policy }));
      const days = policyFrom(result.policy).offline_retention_days;
      if (days === undefined || days <= 0) {
        log.info('a workspace put aside turned offline storage off; deleting its database');
        await deps.wipeOfflineStore(id);
        return;
      }
      await expireIfUnused(id, days, deps);
      return;
    }
    default:
      break;
  }
  // No new policy (a developer sign-in, no answer, nothing to refresh): the
  // retention of the last one this workspace's server sent.
  const days = await storedRetentionDays(id);
  if (days !== null) await expireIfUnused(id, days, deps);
}

async function expireIfUnused(id: string, days: number, deps: CheckInDeps): Promise<void> {
  if (isActive(id)) return;
  if ((await getSecureItemIn(id, SECURE_KEYS.wipePending)) !== null) return;
  if (await deps.expireOfflineStore(id, days)) {
    log.info('a workspace put aside was not used within its offline retention; deleted its database', { days });
  }
}

/** `offline_retention_days` (> 0) of the slot's stored policy, when it is its own origin's; else null. */
async function storedRetentionDays(id: string): Promise<number | null> {
  const raw = await getSecureItemIn(id, SECURE_KEYS.policy);
  if (raw === null) return null;
  try {
    const stored = JSON.parse(raw) as { v?: unknown; origin?: unknown; policy?: unknown };
    if (stored.v !== 1 || typeof stored.policy !== 'object' || stored.policy === null) return null;
    if (stored.origin !== (await getSecureItemIn(id, SECURE_KEYS.serverOrigin))) return null;
    const days = policyFrom(stored.policy as Record<string, unknown>).offline_retention_days;
    return days !== undefined && days > 0 ? days : null;
  } catch {
    return null;
  }
}

let started = false;

/** Starts the check-ins: once the session is hydrated, and on every return to the foreground (idempotent). */
export function startWorkspaceCheckIns(): void {
  if (started) return;
  started = true;
  const run = () => {
    void checkInPutAsideWorkspaces();
  };
  if (getSession().hydrated) run();
  else {
    const off = subscribeSession(() => {
      if (!getSession().hydrated) return;
      off();
      run();
    });
  }
  bindForegroundRefresh({ appState: AppState, refresh: async () => run(), isConnected: () => getSession().hydrated });
}

/** Test helper. */
export function __resetWorkspaceCheckInsForTests(): void {
  lastCheckIn.clear();
  running = null;
}
