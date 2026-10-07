import { useEffect, useState } from 'react';
import { AppState, Platform, StyleSheet, Text, View } from 'react-native';

import { identity } from '@/config/identity';
import { usePalette } from '@/constants/use-palette';
import { coverVisible } from '@/security/privacy/privacyProtection';
import { ramp } from '@/ui';

/**
 * Opaque branded cover shown whenever the app is not active ('inactive' on
 * iOS while the app switcher or a system sheet is up, 'background'). It is
 * the belt to the native braces (iOS app-switcher blur, Android FLAG_SECURE),
 * and the only protection on Android when the policy allows screenshots.
 * It renders above everything, including the lock screen.
 */
export function PrivacyCover() {
  const palette = usePalette();
  const [state, setState] = useState<string | null | undefined>(AppState.currentState);

  useEffect(() => {
    const sub = AppState.addEventListener('change', setState);
    return () => sub.remove();
  }, []);

  if (Platform.OS === 'web' || !coverVisible(state)) return null;
  return (
    <View
      pointerEvents="none"
      style={[StyleSheet.absoluteFill, styles.cover, { backgroundColor: palette.background }]}>
      <Text style={[styles.name, { color: palette.textSecondary }]}>{identity.displayName}</Text>
    </View>
  );
}

const styles = StyleSheet.create({
  cover: { alignItems: 'center', justifyContent: 'center', zIndex: 2000, elevation: 2000 },
  name: { ...ramp.title3, fontWeight: '600' },
});
