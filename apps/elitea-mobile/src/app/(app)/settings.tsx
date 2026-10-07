import { router } from 'expo-router';
import { Alert, Linking, Pressable, ScrollView, StyleSheet, Text, View } from 'react-native';

import { speaksContract } from '@/api/discovery';
import { identity } from '@/config/identity';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { displayHost } from '@/features/connect/connectFlow';
import { UsageCard } from '@/features/usage/UsageCard';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import { usePolicy } from '@/security/policy';
import { useSession } from '@/state/session';
import { useWorkspace } from '@/state/workspace';
import { useWorkspaceInfo } from '@/state/workspaceInfo';
import { useWorkspaceList } from '@/state/workspaceList';
import { useBrandTheme } from '@/theme/brandStore';

export default function SettingsScreen() {
  const palette = usePalette();
  const { serverOrigin, clear } = useWorkspace();
  const credentialKind = useSession((s) => s.credential?.kind ?? null);
  const projectId = useSession((s) => s.projectId);
  const { policy, source, managed } = usePolicy();
  const brand = useBrandTheme();
  const workspaceCount = useWorkspaceList().length;
  const host = serverOrigin ? displayHost(serverOrigin) : null;
  // Client contract 1.3: memory and usage.
  const contract13 = speaksContract(useWorkspaceInfo()?.discovery, 1, 3);

  const onLogOut = () => {
    Alert.alert(
      'Log out & wipe',
      `This signs you out of ${host ?? 'this workspace'} and deletes all data stored on this device for it.${
        workspaceCount > 1 ? ' Your other workspaces stay connected.' : ''
      }`,
      [
        { text: 'Cancel', style: 'cancel' },
        {
          text: 'Log out & wipe',
          style: 'destructive',
          // endSession for THIS workspace: abort in-flight work, server revoke,
          // its encrypted store and secure items; other workspaces are untouched.
          onPress: () => {
            void clear();
          },
        },
      ],
    );
  };

  // The switcher: nothing is wiped, every workspace keeps its data.
  const onSwitchWorkspace = () => {
    router.push('/workspaces');
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
        <View style={styles.workspace}>
          {serverOrigin ? (
            <WorkspaceMark origin={serverOrigin} image={brand?.origin === serverOrigin ? brand.logoMark : null} size={28} />
          ) : null}
          <Text selectable style={[styles.value, styles.flex, { color: palette.text }]}>
            {serverOrigin ?? 'Not connected'}
          </Text>
        </View>
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
        {managed ? (
          <Text testID="policy-managed" style={[styles.label, { color: palette.textSecondary }]}>
            Your organization&apos;s device management makes the lock stricter on this device.
          </Text>
        ) : null}
      </View>

      {contract13 && projectId !== null ? <UsageCard projectId={projectId} /> : null}

      {contract13 && projectId !== null ? (
        <Pressable
          accessibilityRole="button"
          accessibilityHint="What the workspace remembers about you in this project"
          onPress={() => router.push('/memory')}
          style={[styles.card, styles.navRow, { backgroundColor: palette.backgroundElement }]}>
          <Text style={[styles.value, styles.flex, { color: palette.text }]}>Memory</Text>
          <Text style={[styles.value, { color: palette.textSecondary }]}>›</Text>
        </Pressable>
      ) : null}

      <Pressable
        accessibilityRole="button"
        onPress={onSwitchWorkspace}
        style={[styles.button, { borderColor: palette.accent }]}>
        <Text style={[styles.buttonText, { color: palette.accent }]}>
          {workspaceCount > 1 ? `Switch workspace (${workspaceCount})` : 'Add or switch workspace'}
        </Text>
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
  navRow: { flexDirection: 'row', alignItems: 'center' },
  workspace: { flexDirection: 'row', alignItems: 'center', gap: Spacing.two },
  flex: { flex: 1 },
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
