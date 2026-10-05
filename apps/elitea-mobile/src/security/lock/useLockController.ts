import { useEffect, useSyncExternalStore } from 'react';
import { AppState } from 'react-native';

import { appActivity } from '@/lib/appActivity';
import { isEndingSession } from '@/security/endSession';
import { deviceAuth } from '@/security/lock/deviceAuth';
import { LockController, type LockState } from '@/security/lock/lockController';
import { getPolicy, subscribePolicy } from '@/security/policy';

/** The app's one lock controller. */
export const lockController = new LockController({
  deviceAuth,
  getPolicy,
  appState: AppState,
  isDev: __DEV__,
  isSessionEnding: isEndingSession,
  onLockedChange: (locked) => appActivity.setLocked(locked),
  promptMessage: 'Unlock Agent Zefir',
});

// A server policy change (token response, discovery) applies immediately.
subscribePolicy(() => lockController.policyChanged());

/**
 * Binds the lock to the session: guarding starts when a session is signed in
 * and stops when it ends. A session restored at launch locks first; a fresh
 * sign-in does not (the user just authenticated), but the device-security
 * requirement applies to both.
 */
let launchHandled = false;

export function useLockController(signedIn: boolean, hydrated: boolean): LockState {
  useEffect(() => {
    if (!hydrated) return;
    // The first hydrated render decides: signed in already = restored at launch.
    const atLaunch = !launchHandled;
    launchHandled = true;
    if (!signedIn) {
      lockController.stop();
      return;
    }
    void lockController.start(atLaunch ? 'launch' : 'sign_in');
  }, [signedIn, hydrated]);

  return useSyncExternalStore(lockController.subscribe, lockController.getState, lockController.getState);
}
