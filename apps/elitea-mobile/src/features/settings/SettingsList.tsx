import type { ReactNode } from 'react';
import { Pressable, StyleSheet, Switch, View, type AccessibilityActionEvent } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import type { SchemePreference } from '@/design/resolve';
import { COPY_ADDRESS_ACTION, copyWorkspaceAddress, workspaceAddressTitle } from '@/features/workspaces/workspaceAddress';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import type { BrandImage } from '@/theme/brandPack';
import { hitTarget, Icon, ListRow, ListSection, pressedOverlay, ripple, SegmentedControl, Text, uiPlatform } from '@/ui';

export interface SettingsListProps {
  serverOrigin: string | null;
  /** The workspace's brand mark, when its brand is loaded. */
  logoMark: BrandImage | null;
  projectId: number | null;
  /**
   * The selected project as the switcher names it ("My project", or the project's name), never
   * its internal id.
   */
  projectName?: string | null;
  /** Opens the project switcher; no Project row without it. */
  onProject?: (() => void) | null;
  /**
   * The app icon toggle (§3.6): `workspace` = "Zefir Night" in use. Absent where the device
   * cannot change its icon.
   */
  appIcon?: { workspace: boolean; onChange: (workspace: boolean) => void } | null;
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
  projectName,
  onProject,
  appIcon,
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
          subtitle={null}
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
        {onProject && projectId !== null ? (
          <ListRow icon="project" title="Project" value={projectName ?? 'Loading…'} accessibilityHint="Switch project" onPress={onProject} />
        ) : null}
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
        {appIcon ? (
          <SwitchRow
            title="Workspace app icon"
            subtitle={appIcon.workspace ? 'Zefir Night, the neutral icon for self-hosted workspaces' : 'Zefir, the standard icon'}
            value={appIcon.workspace}
            onChange={appIcon.onChange}
          />
        ) : null}
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

/**
 * A row with a trailing switch: the WHOLE row is the control for VoiceOver/TalkBack (role
 * switch, its state checked), and a tap anywhere on it toggles; the switch is the visual.
 */
function SwitchRow({ title, subtitle, value, onChange }: { title: string; subtitle: string; value: boolean; onChange: (next: boolean) => void }) {
  const design = useDesign();
  return (
    <Pressable
      accessibilityRole="switch"
      accessibilityLabel={title}
      accessibilityHint={subtitle}
      accessibilityState={{ checked: value }}
      onPress={() => onChange(!value)}
      android_ripple={ripple(design)}
      style={({ pressed }) => [styles.switchRow, { minHeight: Math.max(hitTarget(), 52) }, pressed && uiPlatform === 'ios' ? { backgroundColor: pressedOverlay(design) } : null]}>
      <Icon name="grid" size={22} tone="accent" />
      <View style={styles.switchText}>
        <Text variant="body">{title}</Text>
        <Text variant="footnote" tone="secondary">
          {subtitle}
        </Text>
      </View>
      <View accessible={false} importantForAccessibility="no-hide-descendants" pointerEvents="none">
        <Switch value={value} trackColor={{ true: design.colors.accent, false: design.colors.separatorStrong }} />
      </View>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  switchRow: { flexDirection: 'row', alignItems: 'center', gap: 12, paddingHorizontal: 16, paddingVertical: 10 },
  switchText: { flex: 1, gap: 2 },
  banner: { borderRadius: 16, borderCurve: 'continuous', padding: 16, gap: 4 },
  bannerTitle: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  segmentRow: { padding: 12 },
});
