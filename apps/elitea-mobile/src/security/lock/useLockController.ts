import { useEffect, useSyncExternalStore } from 'react';
import { AppState } from 'react-native';

import { appActivity } from '@/lib/appActivity';
import { isEndingSession, isSuspendingWorkspace } from '@/security/endSession';
import { deviceAuth } from '@/security/lock/deviceAuth';
import { LockController, type LockState } from '@/security/lock/lockController';
import { getPolicy, subscribePolicy } from '@/security/policy';
import { isSwitchingWorkspace, onWorkspaceSwitch } from '@/state/workspace';

/** The app's one lock controller. */
export const lockController = new LockController({
  deviceAuth,
  getPolicy,
  appState: AppState,
  isDev: __DEV__,
  // A switch resets the policy to the strict defaults in passing: not a tightening.
  isSessionEnding: () => isEndingSession() || isSuspendingWorkspace() || isSwitchingWorkspace(),
  onLockedChange: (locked) => appActivity.setLocked(locked),
  promptMessage: 'Unlock Agent Zefir',
});

// A server policy change (token response, discovery) applies immediately.
subscribePolicy(() => lockController.policyChanged());
// A workspace switch: the next start is a 'switch' (presence carries over or not).
// The mark is dropped when the switch ends signed out (the next start is then a real sign-in).
onWorkspaceSwitch((event) => {
  if (event.phase === 'before') lockController.markSwitch();
  else if (!event.signedIn) lockController.consumeSwitch();
});

/**
 * Binds the lock to the session: guarding starts when a session is signed in
 * and stops when it ends. A session restored at launch locks first; a fresh
 * sign-in does not (the user just authenticated), but the device-security
 * requirement applies to both.
 */
let launchHandled = false;

export function useLockController(signedIn: boolean, hydrated: boolean, workspaceId: string | null = null): LockState {
  // Keyed on the workspace too: a switch between two signed-in workspaces
  // restarts the guard even when React never renders the signed-out moment
  // in between.
  useEffect(() => {
    if (!hydrated) return;
    // The first hydrated render decides: signed in already = restored at launch.
    const atLaunch = !launchHandled;
    launchHandled = true;
    if (!signedIn) {
      lockController.stop();
      return;
    }
    const switched = lockController.consumeSwitch();
    void lockController.start(atLaunch ? 'launch' : switched ? 'switch' : 'sign_in');
  }, [signedIn, hydrated, workspaceId]);

  return useSyncExternalStore(lockController.subscribe, lockController.getState, lockController.getState);
}
