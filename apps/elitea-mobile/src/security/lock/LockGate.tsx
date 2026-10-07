import { Alert, Pressable, StyleSheet, Text, View } from 'react-native';

import { identity } from '@/config/identity';
import { MaxContentWidth, Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import type { LockState } from '@/security/lock/lockController';
import { lockController } from '@/security/lock/useLockController';
import { removeAllWorkspaces } from '@/state/workspace';
import { ramp } from '@/ui';

const FAILURE_TEXT: Record<NonNullable<LockState['lastFailure']>, string> = {
  cancelled: 'Unlock was cancelled.',
  lockout: 'Too many attempts. Unlock your device, then try again.',
  not_available: 'Biometrics and passcode are not available right now.',
  failed: 'Could not verify it was you. Try again.',
};

/**
 * Full-screen opaque cover rendered ABOVE the navigation stack while the app
 * is not unlocked, so navigation state survives a relock. It also blocks all
 * touches to the screens underneath.
 */
export function LockGate({ state }: { state: LockState }) {
  const palette = usePalette();
  if (state.status === 'unlocked') return null;

  const onReset = () => {
    Alert.alert(
      "Can't unlock?",
      `Log out of every workspace and delete everything ${identity.displayName} stored on this device. You can connect and sign in again afterwards.`,
      [
        { text: 'Cancel', style: 'cancel' },
        { text: 'Log out & wipe all', style: 'destructive', onPress: () => void removeAllWorkspaces() },
      ],
    );
  };

  return (
    <View
      style={[StyleSheet.absoluteFill, styles.cover, { backgroundColor: palette.background }]}
      accessibilityViewIsModal
      importantForAccessibility="yes">
      {state.status === 'deciding' ? null : (
        <View style={styles.content}>
          <Text accessibilityRole="header" style={[styles.title, { color: palette.text }]}>
            {state.status === 'no_device_security' ? 'Set a device passcode' : `${identity.displayName} is locked`}
          </Text>
          <Text style={[styles.body, { color: palette.textSecondary }]}>
            {state.status === 'no_device_security'
              ? 'Your workspace requires a device passcode or biometrics. Set one up in your device settings, then come back.'
              : 'Use Face ID, Touch ID, fingerprint or your device passcode to continue.'}
          </Text>
          {state.status === 'locked' && state.lastFailure ? (
            <Text accessibilityRole="alert" style={[styles.body, { color: palette.danger }]}>
              {FAILURE_TEXT[state.lastFailure]}
            </Text>
          ) : null}
          {state.status === 'locked' ? (
            <Pressable
              accessibilityRole="button"
              disabled={state.authInFlight}
              accessibilityState={{ disabled: state.authInFlight, busy: state.authInFlight }}
              onPress={() => void lockController.unlock()}
              style={[styles.button, { backgroundColor: palette.accent, opacity: state.authInFlight ? 0.6 : 1 }]}>
              <Text style={[styles.buttonText, { color: palette.onAccent }]}>Unlock</Text>
            </Pressable>
          ) : null}
          <Pressable
            accessibilityRole="button"
            onPress={onReset}
            style={[styles.button, styles.outline, { borderColor: palette.danger }]}>
            <Text style={[styles.buttonText, { color: palette.danger }]}>
              {state.status === 'no_device_security' ? 'Log out & wipe' : "Can't unlock? Log out & wipe"}
            </Text>
          </Pressable>
          {__DEV__ ? (
            <Pressable
              accessibilityRole="button"
              onPress={() => lockController.bypassForDevelopment()}
              style={styles.linkButton}>
              <Text style={[styles.link, { color: palette.textSecondary }]}>
                Development build: continue without unlocking
              </Text>
            </Pressable>
          ) : null}
        </View>
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  cover: { alignItems: 'center', justifyContent: 'center', padding: Spacing.four, zIndex: 1000, elevation: 1000 },
  content: { width: '100%', maxWidth: MaxContentWidth, gap: Spacing.three, alignItems: 'center' },
  title: { ...ramp.title2, fontWeight: '700', textAlign: 'center' },
  body: { ...ramp.body, textAlign: 'center' },
  button: { alignSelf: 'stretch', borderRadius: 10, paddingVertical: Spacing.three - 2, alignItems: 'center' },
  outline: { borderWidth: 1.5 },
  buttonText: { ...ramp.headline, fontWeight: '600' },
  linkButton: { paddingVertical: Spacing.two },
  link: { ...ramp.footnote },
});
