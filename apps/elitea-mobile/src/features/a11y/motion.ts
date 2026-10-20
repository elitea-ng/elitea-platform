import { useEffect, useState } from 'react';
import { AccessibilityInfo } from 'react-native';

/**
 * Reduce Motion (iOS Settings > Accessibility > Motion), read live: the
 * value changes while the app runs. Sheets and dialogs then appear with a
 * fade or at once instead of sliding.
 */
export function useReducedMotion(): boolean {
  const [reduced, setReduced] = useState(false);
  useEffect(() => {
    let alive = true;
    AccessibilityInfo.isReduceMotionEnabled()
      .then((value) => {
        if (alive) setReduced(value === true);
      })
      .catch(() => {});
    const sub = AccessibilityInfo.addEventListener('reduceMotionChanged', (value) => setReduced(value === true));
    return () => {
      alive = false;
      sub.remove();
    };
  }, []);
  return reduced;
}

export type ModalAnimation = 'none' | 'slide' | 'fade';

/** The Modal `animationType` to use: a slide becomes a fade with Reduce Motion on. */
export function modalAnimation(wanted: ModalAnimation, reduced: boolean): ModalAnimation {
  if (!reduced) return wanted;
  return wanted === 'slide' ? 'fade' : wanted;
}

export function useModalAnimation(wanted: ModalAnimation): ModalAnimation {
  return modalAnimation(wanted, useReducedMotion());
}
