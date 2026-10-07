import { useEffect, useState } from 'react';
import { AppState, Platform, StyleSheet, View } from 'react-native';

import { Mascot } from '@/components/Mascot';
import { identity } from '@/config/identity';
import { coverVisible } from '@/security/privacy/privacyProtection';
import { GradientBackground, Text } from '@/ui';

/**
 * Opaque cover shown whenever the app is not active ('inactive' on
 * iOS while the app switcher or a system sheet is up, 'background'). It is
 * the belt to the native braces (iOS app-switcher blur, Android FLAG_SECURE),
 * and the only protection on Android when the policy allows screenshots.
 * It renders above everything, including the lock screen. It matches the
 * splash (Agent Zefirca's bust on the ground, the app name) so the app-switcher
 * card is recognisable, and carries no content of any workspace (audit 2.7).
 */
export function PrivacyCover() {
  const [state, setState] = useState<string | null | undefined>(AppState.currentState);

  useEffect(() => {
    const sub = AppState.addEventListener('change', setState);
    return () => sub.remove();
  }, []);

  if (Platform.OS === 'web' || !coverVisible(state)) return null;
  return (
    <View pointerEvents="none" style={[StyleSheet.absoluteFill, styles.cover]}>
      <GradientBackground style={styles.center}>
        <Mascot variant="bust" size={96} decorative />
        <Text variant="title3" tone="secondary" emphasized>
          {identity.displayName}
        </Text>
      </GradientBackground>
    </View>
  );
}

const styles = StyleSheet.create({
  cover: { zIndex: 2000, elevation: 2000 },
  center: { alignItems: 'center', justifyContent: 'center', gap: 12 },
});
