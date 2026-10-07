import { useEffect, useState } from 'react';
import { Alert, StyleSheet, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { Mascot } from '@/components/Mascot';
import { identity } from '@/config/identity';
import { MaxContentWidth } from '@/constants/theme';
import { useDesign } from '@/design/DesignProvider';
import { displayHost } from '@/features/connect/connectFlow';
import { useActiveWorkspace, type ActiveWorkspace } from '@/features/workspaces/activeWorkspace';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import { deviceAuth, type Biometry } from '@/security/lock/deviceAuth';
import type { LockState } from '@/security/lock/lockController';
import { lockController } from '@/security/lock/useLockController';
import { removeAllWorkspaces } from '@/state/workspace';
import { Button, GradientBackground, Icon, Text, uiPlatform, type IconName } from '@/ui';

const FAILURE_TEXT: Record<NonNullable<LockState['lastFailure']>, string> = {
  cancelled: 'Unlock was cancelled.',
  lockout: 'Too many attempts. Unlock your device, then try again.',
  not_available: 'Biometrics and passcode are not available right now.',
  failed: 'Could not verify it was you. Try again.',
};

/** The unlock button's words and glyph for the device's biometric (passcode when none). */
export function unlockAction(biometry: Biometry, platform: 'ios' | 'android' = uiPlatform): { title: string; icon: IconName } {
  switch (biometry) {
    case 'face':
      return { title: platform === 'ios' ? 'Unlock with Face ID' : 'Unlock with face', icon: 'faceId' };
    case 'fingerprint':
      return { title: platform === 'ios' ? 'Unlock with Touch ID' : 'Unlock with fingerprint', icon: 'touchId' };
    case 'iris':
      return { title: 'Unlock with iris', icon: 'faceId' };
    default:
      return { title: 'Unlock with passcode', icon: 'passcode' };
  }
}

/**
 * Full-screen opaque cover rendered ABOVE the navigation stack while the app
 * is not unlocked, so navigation state survives a relock. It also blocks all
 * touches to the screens underneath.
 */
export function LockGate({ state }: { state: LockState }) {
  const workspace = useActiveWorkspace();
  const [biometry, setBiometry] = useState<Biometry>(null);
  const locked = state.status !== 'unlocked';
  useEffect(() => {
    if (!locked) return;
    let live = true;
    void deviceAuth.biometry?.().then((kind) => {
      if (live) setBiometry(kind);
    });
    return () => {
      live = false;
    };
  }, [locked]);
  if (!locked) return null;

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
    <LockView
      state={state}
      workspace={workspace}
      biometry={biometry}
      onUnlock={() => void lockController.unlock()}
      onReset={onReset}
      onDevBypass={__DEV__ ? () => lockController.bypassForDevelopment() : null}
    />
  );
}

/**
 * The lock screen (docs/screens.md §9; audit 2.5): the gradient ground, Agent Zefirca's bust,
 * "Agent Zefir is locked", the workspace (logo and host) under it, ONE filled unlock button named
 * for the device's biometric, and the destructive way out as a small plain button at the bottom
 * that asks first. No conversation content is ever drawn here.
 */
export function LockView({
  state,
  workspace,
  biometry,
  onUnlock,
  onReset,
  onDevBypass,
}: {
  state: LockState;
  workspace: ActiveWorkspace | null;
  biometry: Biometry;
  onUnlock: () => void;
  onReset: () => void;
  onDevBypass?: (() => void) | null;
}) {
  const { space } = useDesign();
  const insets = useSafeAreaInsets();
  const action = unlockAction(biometry);
  const noSecurity = state.status === 'no_device_security';
  return (
    <View style={[StyleSheet.absoluteFill, styles.cover]} accessibilityViewIsModal importantForAccessibility="yes" testID="lock-screen">
      <GradientBackground
        style={[
          styles.ground,
          { paddingTop: insets.top + 24, paddingBottom: insets.bottom + 16, paddingHorizontal: space.screenGutter },
        ]}>
        {state.status === 'deciding' ? null : (
          <>
            <View style={styles.content}>
              <Mascot variant="bust" size={96} decorative />
              <Text variant="title2" heading style={styles.center}>
                {noSecurity ? 'Set a device passcode' : `${identity.displayName} is locked`}
              </Text>
              {workspace ? (
                <View style={styles.workspace} accessible accessibilityLabel={`Workspace ${workspace.name ? `${workspace.name}, ` : ''}${displayHost(workspace.origin)}`}>
                  <WorkspaceMark origin={workspace.origin} image={workspace.logoMark} brand={workspace.brand} size={20} />
                  <Text variant="footnote" tone="secondary" numberOfLines={2} style={styles.shrink}>
                    {displayHost(workspace.origin)}
                  </Text>
                </View>
              ) : null}
              <Text variant="callout" tone="secondary" style={styles.center}>
                {noSecurity
                  ? 'Your workspace requires a device passcode or biometrics. Set one up in your device settings, then come back.'
                  : 'Your chats stay hidden until you unlock.'}
              </Text>
              {state.status === 'locked' && state.lastFailure ? (
                <View style={styles.failure}>
                  <Icon name="error" size={16} tone="danger" />
                  <Text accessibilityRole="alert" variant="subheadline" tone="danger" style={styles.shrink}>
                    {FAILURE_TEXT[state.lastFailure]}
                  </Text>
                </View>
              ) : null}
              {state.status === 'locked' ? (
                <Button testID="unlock" title={action.title} icon={action.icon} loading={state.authInFlight} onPress={onUnlock} />
              ) : null}
            </View>
            <View style={styles.bottom}>
              <Button
                title={noSecurity ? 'Log out & wipe' : "Can't unlock?"}
                variant="destructivePlain"
                size="small"
                accessibilityHint="Logs out of every workspace and deletes their data on this device, after you confirm"
                onPress={onReset}
              />
              {onDevBypass ? (
                <Button title="Development build: continue without unlocking" variant="plain" size="small" onPress={onDevBypass} />
              ) : null}
            </View>
          </>
        )}
      </GradientBackground>
    </View>
  );
}

const styles = StyleSheet.create({
  cover: { zIndex: 1000, elevation: 1000 },
  ground: { alignItems: 'center', justifyContent: 'space-between' },
  content: { flex: 1, width: '100%', maxWidth: MaxContentWidth, gap: 14, alignItems: 'center', justifyContent: 'center' },
  center: { textAlign: 'center' },
  workspace: { flexDirection: 'row', alignItems: 'center', gap: 6, maxWidth: '100%' },
  failure: { flexDirection: 'row', alignItems: 'flex-start', gap: 6 },
  shrink: { flexShrink: 1 },
  bottom: { alignItems: 'center', gap: 4 },
});
