import { useEffect, useState } from 'react';
import { AccessibilityInfo } from 'react-native';

/**
 * VoiceOver / TalkBack running, read live. Controls that are otherwise
 * revealed by a gesture (an older answer's action row) stay drawn while it
 * is on, so nothing is reachable only by a long press.
 */
export function useScreenReader(): boolean {
  const [on, setOn] = useState(false);
  useEffect(() => {
    let alive = true;
    AccessibilityInfo.isScreenReaderEnabled?.()
      .then((value) => {
        if (alive) setOn(value === true);
      })
      .catch(() => {});
    const sub = AccessibilityInfo.addEventListener?.('screenReaderChanged', (value) => setOn(value === true));
    return () => {
      alive = false;
      sub?.remove();
    };
  }, []);
  return on;
}
