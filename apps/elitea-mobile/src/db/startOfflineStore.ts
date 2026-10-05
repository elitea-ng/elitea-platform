import { Platform } from 'react-native';

import { bindOfflineStore } from '@/db/offlineBinding';
import { offlineStore } from '@/db/offlineStore';
import { appActivity } from '@/lib/appActivity';
import { getPolicyState, subscribePolicy } from '@/security/policy';
import { lockController } from '@/security/lock/useLockController';
import { getSession, subscribeSession } from '@/state/session';

let started = false;

/** Wires the app's offline store to the session, the policy and the device lock. Idempotent. */
export function startOfflineStore(): void {
  if (started) return;
  started = true;
  bindOfflineStore({
    store: offlineStore,
    session: { get: getSession, subscribe: subscribeSession },
    policy: { get: getPolicyState, subscribe: subscribePolicy },
    lock: { get: lockController.getState, subscribe: lockController.subscribe },
    activity: appActivity,
    supported: Platform.OS !== 'web',
  });
}
