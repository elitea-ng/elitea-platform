import { router } from 'expo-router';
import { Pressable, StyleSheet, View } from 'react-native';

import { displayHost } from '@/features/connect/connectFlow';
import { useActiveWorkspace } from '@/features/workspaces/activeWorkspace';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import { Icon, Text } from '@/ui';

/**
 * Header title of every signed-in screen: the screen title over the ACTIVE
 * workspace's host, with its mark (in the workspace's accent when it has no
 * logo). The host is always on screen, so the user always knows which
 * workspace they are typing into (two workspaces may share a brand). Tapping
 * it opens the workspace switcher.
 */
export function WorkspaceTitle({ title }: { title: string }) {
  const workspace = useActiveWorkspace();
  if (workspace === null) {
    return (
      <Text variant="headline" emphasized>
        {title}
      </Text>
    );
  }
  const host = displayHost(workspace.origin);
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={`${title}. Workspace ${host}. Switch workspace`}
      testID="workspace-title"
      onPress={() => router.push('/workspaces')}
      style={styles.box}>
      <Text variant="headline" emphasized numberOfLines={1}>
        {title}
      </Text>
      <View style={styles.hostRow}>
        <WorkspaceMark origin={workspace.origin} image={workspace.logoMark} brand={workspace.brand} size={14} />
        <Text variant="caption" tone="secondary" numberOfLines={1}>
          {host}
        </Text>
        <Icon name="chevronDown" size={10} tone="secondary" weight="semibold" />
      </View>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  box: { alignItems: 'center', maxWidth: 220 },
  hostRow: { flexDirection: 'row', alignItems: 'center', gap: 4 },
});
