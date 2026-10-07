import { router } from 'expo-router';
import * as WebBrowser from 'expo-web-browser';
import { Alert, ScrollView, StyleSheet } from 'react-native';

import { speaksContract } from '@/api/discovery';
import { identity } from '@/config/identity';
import { useDesign } from '@/design/DesignProvider';
import { displayHost } from '@/features/connect/connectFlow';
import { SettingsList } from '@/features/settings/SettingsList';
import { UsageCard } from '@/features/usage/UsageCard';
import { usePolicy } from '@/security/policy';
import { useSession } from '@/state/session';
import { useWorkspace } from '@/state/workspace';
import { useWorkspaceInfo } from '@/state/workspaceInfo';
import { useWorkspaceList } from '@/state/workspaceList';
import { setSchemePreference, useSchemePreference } from '@/state/appearance';
import { useBrandTheme } from '@/theme/brandStore';

export default function SettingsScreen() {
  const { colors, space } = useDesign();
  const scheme = useSchemePreference();
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

  const lockText = policy.require_device_lock
    ? policy.idle_lock_seconds === 0
      ? 'When the app starts'
      : `When the app starts and after ${formatIdle(policy.idle_lock_seconds)} in the background`
    : 'Not required';

  return (
    <ScrollView
      contentInsetAdjustmentBehavior="automatic"
      style={{ backgroundColor: colors.background }}
      contentContainerStyle={[styles.content, { paddingHorizontal: space.screenGutterCompact }]}>
      <SettingsList
        serverOrigin={serverOrigin}
        logoMark={serverOrigin !== null && brand?.origin === serverOrigin ? brand.logoMark : null}
        projectId={projectId}
        workspaceCount={workspaceCount}
        developerSignIn={credentialKind === 'pat'}
        scheme={scheme}
        onSchemeChange={(next) => void setSchemePreference(next)}
        lockText={lockText}
        screenshotsAllowed={policy.allow_screenshots}
        securityFooter={`${POLICY_SOURCE_TEXT[source]}${managed ? ' ' + MANAGED_TEXT : ''}`}
        usage={contract13 && projectId !== null ? <UsageCard projectId={projectId} /> : null}
        onMemory={contract13 && projectId !== null ? () => router.push('/memory') : null}
        onSwitchWorkspace={onSwitchWorkspace}
        onPrivacy={() => void WebBrowser.openBrowserAsync(identity.privacyUrl)}
        onSupport={() => void WebBrowser.openBrowserAsync(identity.supportUrl)}
        onDesignGallery={__DEV__ ? () => router.push('/design-gallery') : null}
        onLogOut={onLogOut}
      />
    </ScrollView>
  );
}

const MANAGED_TEXT = "Your organization's device management makes the lock stricter on this device.";

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
  content: { paddingVertical: 16, gap: 28 },
});
