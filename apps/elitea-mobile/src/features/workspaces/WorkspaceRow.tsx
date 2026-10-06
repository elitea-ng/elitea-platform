import { Pressable, StyleSheet, Text, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { displayHost } from '@/features/connect/connectFlow';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import type { WorkspaceSummary } from '@/state/workspaceList';

/** One connected workspace: mark, name (when branded), host (always), state. */
export function WorkspaceRow({
  workspace,
  active,
  onPress,
  onRemove,
  disabled = false,
}: {
  workspace: WorkspaceSummary;
  active: boolean;
  onPress: () => void;
  onRemove?: () => void;
  disabled?: boolean;
}) {
  const palette = usePalette();
  const host = displayHost(workspace.origin);
  const state = active ? 'Active' : workspace.signedIn ? 'Signed in' : 'Signed out';
  return (
    <View style={styles.row}>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={`${workspace.name ? `${workspace.name}, ` : ''}${host}. ${state}`}
        accessibilityState={{ selected: active, disabled }}
        testID={`workspace-${workspace.id}`}
        disabled={disabled}
        onPress={onPress}
        style={styles.main}>
        <WorkspaceMark origin={workspace.origin} image={workspace.logoMark} size={36} />
        <View style={styles.text}>
          {workspace.name ? (
            <Text numberOfLines={1} style={[styles.name, { color: palette.text }]}>
              {workspace.name}
            </Text>
          ) : null}
          <Text
            numberOfLines={1}
            style={[workspace.name ? styles.host : styles.name, { color: workspace.name ? palette.textSecondary : palette.text }]}>
            {host}
          </Text>
          <Text style={[styles.state, { color: active ? palette.accent : palette.textSecondary }]}>{state}</Text>
        </View>
        {active ? <Text style={{ color: palette.accent, fontSize: 18 }}>✓</Text> : null}
      </Pressable>
      {onRemove ? (
        <Pressable
          accessibilityRole="button"
          accessibilityLabel={`Remove ${host}`}
          testID={`remove-${workspace.id}`}
          hitSlop={8}
          onPress={onRemove}
          style={styles.remove}>
          <Text style={{ color: palette.danger, fontSize: 15 }}>Remove</Text>
        </Pressable>
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  row: { flexDirection: 'row', alignItems: 'center', paddingHorizontal: Spacing.three },
  main: { flex: 1, flexDirection: 'row', alignItems: 'center', gap: Spacing.three, paddingVertical: Spacing.three },
  text: { flex: 1, gap: 2 },
  name: { fontSize: 16, fontWeight: '600' },
  host: { fontSize: 14 },
  state: { fontSize: 12, fontWeight: '600' },
  remove: { paddingLeft: Spacing.three, paddingVertical: Spacing.two },
});
