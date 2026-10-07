import { Pressable, StyleSheet, View } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { displayHost } from '@/features/connect/connectFlow';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import type { SwipeRowA11y } from '@/components/SwipeToDelete';
import type { WorkspaceSummary } from '@/state/workspaceList';
import type { BrandImage } from '@/theme/brandPack';
import { hitTarget, Icon, pressedOverlay, ripple, Text, uiPlatform } from '@/ui';

/**
 * The mark a row may show. A workspace that is not active is never contacted
 * just to draw its row (WS-SEC-4: that would tell a put-aside workspace's
 * server when, and from where, this device lists it): only a mark already on
 * the device (a data: URI) is shown, otherwise the host's initial on a tile in
 * that workspace's own accent.
 */
export function rowMark(image: BrandImage | null, active: boolean): BrandImage | null {
  if (image === null || active) return image;
  return image.uri.startsWith('data:') ? image : null;
}

/** The row's state line: never colour alone (the word says it). */
export function workspaceState(workspace: Pick<WorkspaceSummary, 'signedIn'>, active: boolean): string {
  return active ? 'Active' : workspace.signedIn ? 'Signed in' : 'Signed out';
}

/**
 * One connected workspace (an inset-grouped row, §4.10 sheet content): its mark in its own
 * accent, the brand name when it has one, the host (always, system font) and its state, a check
 * on the active one. The whole row is the switch target; removing is a trailing swipe (the
 * sheet wraps the row in `SwipeToDelete`) and the same named accessibility action, carried in
 * `a11y` (EH-7: a red "Remove" on every row made the picker read as a management screen).
 */
export function WorkspaceRow({
  workspace,
  active,
  onPress,
  a11y,
  disabled = false,
}: {
  workspace: WorkspaceSummary;
  active: boolean;
  onPress: () => void;
  /** The swipe row's accessibility action ("Remove") and busy state. */
  a11y?: SwipeRowA11y;
  disabled?: boolean;
}) {
  const design = useDesign();
  const host = displayHost(workspace.origin);
  const state = workspaceState(workspace, active);
  return (
    <View style={[styles.row, { backgroundColor: design.colors.surface }]}>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={`${workspace.name ? `${workspace.name}, ` : ''}${host}. ${state}`}
        accessibilityState={{ selected: active, disabled, busy: a11y?.accessibilityState?.busy }}
        accessibilityActions={a11y?.accessibilityActions}
        onAccessibilityAction={a11y?.onAccessibilityAction}
        testID={`workspace-${workspace.id}`}
        disabled={disabled}
        onPress={onPress}
        android_ripple={ripple(design)}
        style={({ pressed }) => [
          styles.main,
          { minHeight: Math.max(hitTarget(), 60) },
          pressed && uiPlatform === 'ios' ? { backgroundColor: pressedOverlay(design) } : null,
          disabled ? styles.disabled : null,
        ]}>
        <WorkspaceMark origin={workspace.origin} image={rowMark(workspace.logoMark, active)} brand={workspace.brand} size={36} />
        <View style={styles.text}>
          {workspace.name ? (
            <Text variant="headline" numberOfLines={design.accessibilityLayout ? undefined : 1}>
              {workspace.name}
            </Text>
          ) : null}
          {/* The host in the system font (a brand font must not be able to disguise it). */}
          <Text variant={workspace.name ? 'subheadline' : 'headline'} tone={workspace.name ? 'secondary' : 'primary'} numberOfLines={design.accessibilityLayout ? undefined : 1}>
            {host}
          </Text>
          <Text variant="footnote" tone={active ? 'accent' : 'secondary'}>
            {state}
          </Text>
        </View>
        {active ? <Icon name="check" size={20} tone="accent" weight="semibold" /> : null}
      </Pressable>
    </View>
  );
}

const styles = StyleSheet.create({
  row: { flexDirection: 'row', alignItems: 'center' },
  main: { flex: 1, flexDirection: 'row', alignItems: 'center', gap: 12, paddingHorizontal: 16, paddingVertical: 10 },
  text: { flex: 1, gap: 2 },
  disabled: { opacity: 0.38 },
});
