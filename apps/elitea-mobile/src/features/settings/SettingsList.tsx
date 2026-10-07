import type { ReactNode } from 'react';
import { StyleSheet, View, type AccessibilityActionEvent } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import type { SchemePreference } from '@/design/resolve';
import { COPY_ADDRESS_ACTION, copyWorkspaceAddress, workspaceAddressTitle } from '@/features/workspaces/workspaceAddress';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import type { BrandImage } from '@/theme/brandPack';
import { Icon, ListRow, ListSection, SegmentedControl, Text } from '@/ui';

export interface SettingsListProps {
  serverOrigin: string | null;
  /** The workspace's brand mark, when its brand is loaded. */
  logoMark: BrandImage | null;
  projectId: number | null;
  workspaceCount: number;
  /** Signed in with a personal access token (development): the warning banner. */
  developerSignIn: boolean;
  scheme: SchemePreference;
  onSchemeChange: (next: SchemePreference) => void;
  lockText: string;
  screenshotsAllowed: boolean;
  securityFooter: string;
  /** The usage card (client contract 1.3 with a project), or nothing. */
  usage?: ReactNode;
  /** Opens Memory (client contract 1.3 with a project); no row without it. */
  onMemory?: (() => void) | null;
  onSwitchWorkspace: () => void;
  onPrivacy: () => void;
  onSupport: () => void;
  /** Development builds: the design gallery row. */
  onDesignGallery?: (() => void) | null;
  onLogOut: () => void;
}

/**
 * The Settings list, fed by the Settings tab (live state) and by its `__DEV__` preview
 * (`/design-tabs/settings`, sample values), so the review screenshots show the screen the user
 * gets (D2). Rendered inside the tab's root scroll view.
 */
export function SettingsList({
  serverOrigin,
  logoMark,
  projectId,
  workspaceCount,
  developerSignIn,
  scheme,
  onSchemeChange,
  lockText,
  screenshotsAllowed,
  securityFooter,
  usage,
  onMemory,
  onSwitchWorkspace,
  onPrivacy,
  onSupport,
  onDesignGallery,
  onLogOut,
}: SettingsListProps) {
  const { colors } = useDesign();
  return (
    <>
      {developerSignIn ? (
        <View accessibilityRole="alert" style={[styles.banner, { backgroundColor: colors.dangerSoft }]}>
          <View style={styles.bannerTitle}>
            <Icon name="warning" size={18} tone="danger" />
            <Text variant="headline" tone="danger">
              Developer sign-in
            </Text>
          </View>
          <Text variant="footnote" tone="secondary">
            You are signed in with a personal access token. Use this only for development and testing.
          </Text>
        </View>
      ) : null}

      <ListSection title="Workspace">
        <ListRow
          title={serverOrigin ? workspaceAddressTitle(serverOrigin) : 'Not connected'}
          subtitle={projectId !== null ? `Project #${projectId}` : null}
          leading={serverOrigin ? <WorkspaceMark origin={serverOrigin} image={logoMark} size={28} /> : undefined}
          accessibilityLabel={`Workspace ${serverOrigin ?? 'not connected'}`}
          {...(serverOrigin
            ? {
                // The full address (with its scheme) can be copied for support: long press, or
                // the "Copy address" action for VoiceOver/TalkBack.
                onLongPress: () => void copyWorkspaceAddress(serverOrigin),
                accessibilityHint: 'Long press to copy the address',
                accessibilityActions: [COPY_ADDRESS_ACTION],
                onAccessibilityAction: (event: AccessibilityActionEvent) => {
                  if (event.nativeEvent.actionName === COPY_ADDRESS_ACTION.name) void copyWorkspaceAddress(serverOrigin);
                },
              }
            : null)}
        />
        <ListRow
          icon="switchWorkspace"
          title={workspaceCount > 1 ? `Switch workspace (${workspaceCount})` : 'Add or switch workspace'}
          onPress={onSwitchWorkspace}
        />
      </ListSection>

      <ListSection title="Appearance" footer="Your workspace may change the accent colour, never the light or dark choice.">
        <View style={styles.segmentRow}>
          <SegmentedControl
            accessibilityLabel="Appearance"
            value={scheme}
            onChange={onSchemeChange}
            segments={[
              { value: 'system', label: 'System' },
              { value: 'light', label: 'Light' },
              { value: 'dark', label: 'Dark' },
            ]}
          />
        </View>
      </ListSection>

      <ListSection title="Security" footer={securityFooter}>
        <ListRow icon="lock" title="Device lock" subtitle={lockText} />
        <ListRow icon="image" title="Screenshots" subtitle={screenshotsAllowed ? 'Allowed' : 'Blocked where the device allows it'} />
      </ListSection>

      {usage ?? null}

      {onMemory ? (
        <ListSection>
          <ListRow icon="memory" title="Memory" accessibilityHint="What the workspace remembers about you in this project" onPress={onMemory} />
        </ListSection>
      ) : null}

      <ListSection title="About">
        <ListRow icon="info" title="Privacy policy" onPress={onPrivacy} />
        <ListRow icon="link" title="Support" onPress={onSupport} />
        {onDesignGallery ? <ListRow icon="palette" title="Design gallery" subtitle="Development builds only" onPress={onDesignGallery} /> : null}
      </ListSection>

      <ListSection>
        <ListRow title="Log out & wipe" destructive onPress={onLogOut} />
      </ListSection>
    </>
  );
}

const styles = StyleSheet.create({
  banner: { borderRadius: 16, borderCurve: 'continuous', padding: 16, gap: 4 },
  bannerTitle: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  segmentRow: { padding: 12 },
});
