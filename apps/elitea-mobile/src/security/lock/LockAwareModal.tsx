import { Modal, type ModalProps } from 'react-native';

import { useLockCovers } from '@/security/lock/useLockController';

/**
 * The app's only Modal. A React Native Modal is drawn in its own native
 * window (a presented view controller on iOS, a Dialog on Android), ABOVE the
 * root view where LockGate's cover lives, so a sheet left open when the app
 * relocks would stay usable on top of the lock screen (Copy, Share, Read
 * aloud, a HITL approval). This one is hidden while the lock covers the app
 * and comes back, as it was asked for, after the unlock.
 * src/security/lock/lockCover.test.tsx fails on any other Modal import.
 */
export function LockAwareModal(props: ModalProps) {
  const covered = useLockCovers();
  return <Modal {...props} visible={(props.visible ?? true) && !covered} />;
}
