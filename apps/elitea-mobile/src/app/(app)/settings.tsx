import { Alert, Linking, Pressable, ScrollView, StyleSheet, Text, View } from 'react-native';

import { identity } from '@/config/identity';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { endSession } from '@/security/endSession';
import { usePolicy } from '@/security/policy';
import { useSession } from '@/state/session';
import { useWorkspace } from '@/state/workspace';

export default function SettingsScreen() {
  const palette = usePalette();
  const { serverOrigin, clear } = useWorkspace();
  const credentialKind = useSession((s) => s.credential?.kind ?? null);
  const projectId = useSession((s) => s.projectId);
  const { policy, source } = usePolicy();

  const onLogOut = () => {
    Alert.alert(
      'Log out & wipe',
      'This signs you out and deletes all data stored on this device for this workspace.',
      [
        { text: 'Cancel', style: 'cancel' },
        {
          text: 'Log out & wipe',
          style: 'destructive',
          // endSession: abort in-flight work, wipe caches and every secure-store
          // key, reset the session. B3/B4 add server revoke and the encrypted store.
          onPress: () => {
            void clear();
          },
        },
      ],
    );
  };

  const onSwitchWorkspace = () => {
    Alert.alert(
      'Switch workspace?',
      `This signs you out of ${serverOrigin ?? 'this workspace'} and deletes everything ${identity.displayName} stored for it on this device.`,
      [
        { text: 'Cancel', style: 'cancel' },
        {
          text: 'Switch & wipe',
          style: 'destructive',
          onPress: () => {
            void endSession({ reason: 'server_switch' });
          },
        },
      ],
    );
  };

  return (
    <ScrollView
      style={{ backgroundColor: palette.background }}
      contentContainerStyle={styles.content}>
      {credentialKind === 'pat' ? (
        <View
          accessibilityRole="alert"
          style={[styles.banner, { borderColor: palette.danger }]}>
          <Text style={[styles.bannerTitle, { color: palette.danger }]}>Developer sign-in</Text>
          <Text style={[styles.bannerBody, { color: palette.textSecondary }]}>
            You are signed in with a personal access token. Use this only for development and
            testing.
          </Text>
        </View>
      ) : null}

      <View style={[styles.card, { backgroundColor: palette.backgroundElement }]}>
        <Text style={[styles.label, { color: palette.textSecondary }]}>Workspace</Text>
        <Text selectable style={[styles.value, { color: palette.text }]}>
          {serverOrigin ?? 'Not connected'}
        </Text>
        {projectId !== null ? (
          <Text style={[styles.label, { color: palette.textSecondary }]}>Project #{projectId}</Text>
        ) : null}
      </View>

      <View style={[styles.card, { backgroundColor: palette.backgroundElement }]}>
        <Text style={[styles.label, { color: palette.textSecondary }]}>Device lock</Text>
        <Text style={[styles.value, { color: palette.text }]}>
          {policy.require_device_lock
            ? policy.idle_lock_seconds === 0
              ? 'When the app starts'
              : `When the app starts and after ${formatIdle(policy.idle_lock_seconds)} in the background`
            : 'Not required'}
        </Text>
        <Text style={[styles.label, { color: palette.textSecondary }]}>Screenshots</Text>
        <Text style={[styles.value, { color: palette.text }]}>
          {policy.allow_screenshots ? 'Allowed' : 'Blocked where the device allows it'}
        </Text>
        <Text style={[styles.label, { color: palette.textSecondary }]}>
          {POLICY_SOURCE_TEXT[source]}
        </Text>
      </View>

      <Pressable
        accessibilityRole="button"
        onPress={onSwitchWorkspace}
        style={[styles.button, { borderColor: palette.accent }]}>
        <Text style={[styles.buttonText, { color: palette.accent }]}>Switch workspace</Text>
      </Pressable>

      <Pressable
        accessibilityRole="button"
        onPress={onLogOut}
        style={[styles.button, { borderColor: palette.danger }]}>
        <Text style={[styles.buttonText, { color: palette.danger }]}>Log out & wipe</Text>
      </Pressable>

      <View style={styles.links}>
        <Text
          accessibilityRole="link"
          onPress={() => Linking.openURL(identity.privacyUrl)}
          style={[styles.link, { color: palette.accent }]}>
          Privacy policy
        </Text>
        <Text
          accessibilityRole="link"
          onPress={() => Linking.openURL(identity.supportUrl)}
          style={[styles.link, { color: palette.accent }]}>
          Support
        </Text>
      </View>
    </ScrollView>
  );
}

const POLICY_SOURCE_TEXT = {
  default: 'Security settings: strict defaults (the workspace has not sent a policy)',
  discovery: "Security settings: the workspace's public policy, strict otherwise",
  server: 'Security settings: set by the workspace',
} as const;

function formatIdle(seconds: number): string {
  if (seconds % 60 === 0) {
    const minutes = seconds / 60;
    return minutes === 1 ? '1 minute' : `${minutes} minutes`;
  }
  return seconds === 1 ? '1 second' : `${seconds} seconds`;
}

const styles = StyleSheet.create({
  content: { padding: Spacing.three, gap: Spacing.three },
  card: { borderRadius: 12, padding: Spacing.three, gap: Spacing.one },
  banner: { borderWidth: 1, borderRadius: 12, padding: Spacing.three, gap: Spacing.one },
  bannerTitle: { fontSize: 14, fontWeight: '700' },
  bannerBody: { fontSize: 13, lineHeight: 18 },
  label: { fontSize: 13, fontWeight: '600' },
  value: { fontSize: 16 },
  button: {
    borderWidth: 1.5,
    borderRadius: 10,
    paddingVertical: Spacing.three - 2,
    alignItems: 'center',
  },
  buttonText: { fontSize: 16, fontWeight: '600' },
  links: { flexDirection: 'row', gap: Spacing.four, justifyContent: 'center' },
  link: { fontSize: 15 },
});
