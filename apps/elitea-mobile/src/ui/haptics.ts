import * as Haptics from 'expo-haptics';
import { Platform } from 'react-native';

import { haptics as HAPTIC_EVENTS } from '@/design/tokens';

/**
 * Haptic feedback per user-initiated event (docs/design-system.md §1.10). Never for streaming
 * tokens or incoming notifications. The OS setting mutes them; there is no in-app toggle. A
 * device without a haptic engine, or a failing call, is silently ignored.
 */
export type HapticEvent = keyof typeof HAPTIC_EVENTS;

export function haptic(event: HapticEvent): void {
  const kind = HAPTIC_EVENTS[event];
  let pending: Promise<void>;
  try {
    pending = run(kind);
  } catch {
    return;
  }
  pending.catch(() => {});
}

function run(kind: (typeof HAPTIC_EVENTS)[HapticEvent]): Promise<void> {
  switch (kind) {
    case 'selection':
      return Haptics.selectionAsync();
    case 'impact.light':
      return Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light);
    case 'impact.medium':
      return Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Medium);
    case 'impact.rigid':
      // Android: the system "confirm" constant where the platform has one (§1.10).
      return Platform.OS === 'android'
        ? Haptics.performAndroidHapticsAsync(Haptics.AndroidHaptics.Confirm)
        : Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Rigid);
    case 'notification.success':
      return Haptics.notificationAsync(Haptics.NotificationFeedbackType.Success);
    case 'notification.warning':
      return Haptics.notificationAsync(Haptics.NotificationFeedbackType.Warning);
    case 'notification.error':
      return Haptics.notificationAsync(Haptics.NotificationFeedbackType.Error);
  }
}
