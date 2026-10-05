import type { AppStateStatus } from 'react-native';

import type { OfflineStore } from '@/db/offlineStore';
import { log } from '@/lib/log';
import type { PolicyState } from '@/security/policy';
import type { LockState } from '@/security/lock/lockController';
import type { SessionState } from '@/state/session';

/**
 * Decides when the offline store is open, closed or wiped:
 *
 * | condition                                   | store  |
 * |---------------------------------------------|--------|
 * | signed out, or the session is ending         | closed (endSession wipes it) |
 * | policy has no offline storage                | wiped, never created |
 * | locked (or the lock not decided yet)         | closed, key not in memory |
 * | signed in, unlocked, offline allowed         | open   |
 *
 * Offline storage is allowed only by a server policy with
 * `offline_retention_days > 0` (the strict default is 0, and a discovery
 * document that says `offline_enabled: false` turns it off, see
 * policyStore.ts). The store opening only while the lock controller says
 * `unlocked` is the key release gate: under `require_device_lock` that is
 * after biometrics or the device passcode.
 */

export function offlineAllowed(state: PolicyState): boolean {
  return state.source === 'server' && state.policy.offline_retention_days > 0;
}

interface Source<T> {
  get(): T;
  subscribe(listener: () => void): () => void;
}

export interface OfflineBindingDeps {
  store: Pick<OfflineStore, 'open' | 'close' | 'wipe' | 'retain' | 'setKeyGate'>;
  session: Source<SessionState>;
  policy: Source<PolicyState>;
  lock: Source<LockState>;
  activity?: { addEventListener(type: 'change', listener: (state: AppStateStatus) => void): { remove(): void } };
  /** False on web (a development surface; no SQLCipher there). */
  supported: boolean;
}

export function bindOfflineStore(deps: OfflineBindingDeps): () => void {
  const { store } = deps;
  const unlocked = () => deps.lock.get().status === 'unlocked';
  store.setKeyGate(unlocked);

  let wipedWhileDisabled = false;
  let lastPolicy = deps.policy.get().policy;

  const reconcile = () => {
    const session = deps.session.get();
    const policyState = deps.policy.get();
    const signedIn = session.serverOrigin !== null && session.credential !== null && !session.ending;
    if (!deps.supported || !signedIn) {
      void store.close();
      return;
    }
    if (!offlineAllowed(policyState)) {
      if (!wipedWhileDisabled) {
        wipedWhileDisabled = true;
        store.wipe().catch((error: unknown) => log.warn('offline store wipe failed', { error }));
      }
      return;
    }
    wipedWhileDisabled = false;
    if (!unlocked()) {
      void store.close();
      return;
    }
    store.open(session.serverOrigin as string).catch((error: unknown) => log.warn('offline store open failed', { error }));
    if (policyState.policy !== lastPolicy) {
      lastPolicy = policyState.policy;
      void store.retain();
    }
  };

  const subs = [deps.session.subscribe(reconcile), deps.policy.subscribe(reconcile), deps.lock.subscribe(reconcile)];
  const activity = deps.activity?.addEventListener('change', (state) => {
    if (state === 'active') void store.retain();
  });
  reconcile();
  return () => {
    for (const unsubscribe of subs) unsubscribe();
    activity?.remove();
    store.setKeyGate(() => false);
  };
}
