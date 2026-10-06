import { router } from 'expo-router';
import { useEffect } from 'react';
import { Alert, FlatList, Pressable, StyleSheet, Text, View } from 'react-native';

import { identity } from '@/config/identity';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { displayHost } from '@/features/connect/connectFlow';
import { WorkspaceRow } from '@/features/workspaces/WorkspaceRow';
import { log } from '@/lib/log';
import { MAX_WORKSPACES } from '@/security/workspaceRegistry';
import { useSession } from '@/state/session';
import { beginAddWorkspace, removeWorkspace, switchToWorkspace } from '@/state/workspace';
import { refreshWorkspaceList, useWorkspaceList, type WorkspaceSummary } from '@/state/workspaceList';

/**
 * The workspace switcher (modal): every workspace connected on this device,
 * each with its host. Switching never deletes anything; "Remove" logs out of
 * that one workspace and deletes only its data; "Add workspace" opens the
 * connect flow with the current workspace kept.
 */
export default function WorkspacesScreen() {
  const palette = usePalette();
  const activeId = useSession((s) => s.workspaceId);
  const workspaces = useWorkspaceList();

  useEffect(() => {
    void refreshWorkspaceList();
  }, []);

  const onOpen = (workspace: WorkspaceSummary) => {
    if (router.canGoBack()) router.back();
    if (workspace.id === activeId) return;
    void switchToWorkspace(workspace.id).then((ok) => {
      if (!ok) log.warn('workspace could not be opened');
    });
  };

  const onRemove = (workspace: WorkspaceSummary) => {
    const host = displayHost(workspace.origin);
    const active = workspace.id === activeId;
    Alert.alert(
      `Remove ${host}?`,
      `This signs you out of ${host} and deletes everything ${identity.displayName} stored for it on this device.${
        workspaces.length > 1 ? ' Your other workspaces are not affected.' : ''
      }`,
      [
        { text: 'Cancel', style: 'cancel' },
        {
          text: 'Log out & remove',
          style: 'destructive',
          onPress: () => {
            if (active && router.canGoBack()) router.back();
            void removeWorkspace(workspace.id);
          },
        },
      ],
    );
  };

  const full = workspaces.length >= MAX_WORKSPACES;
  return (
    <FlatList
      style={{ backgroundColor: palette.background }}
      data={workspaces}
      keyExtractor={(w) => w.id}
      ItemSeparatorComponent={() => <View style={[styles.separator, { backgroundColor: palette.border }]} />}
      renderItem={({ item }) => (
        <WorkspaceRow
          workspace={item}
          active={item.id === activeId}
          onPress={() => onOpen(item)}
          onRemove={() => onRemove(item)}
        />
      )}
      ListFooterComponent={
        <View style={styles.footer}>
          <Pressable
            accessibilityRole="button"
            testID="add-workspace"
            disabled={full}
            accessibilityState={{ disabled: full }}
            onPress={() => {
              if (router.canGoBack()) router.back();
              void beginAddWorkspace();
            }}
            style={[styles.button, { borderColor: palette.accent, opacity: full ? 0.5 : 1 }]}>
            <Text style={[styles.buttonText, { color: palette.accent }]}>Add workspace</Text>
          </Pressable>
          <Text style={[styles.hint, { color: palette.textSecondary }]}>
            {full
              ? `At most ${MAX_WORKSPACES} workspaces can be connected. Remove one to add another.`
              : 'Each workspace keeps its own sign-in, data and settings on this device. Switching never deletes anything.'}
          </Text>
        </View>
      }
    />
  );
}

const styles = StyleSheet.create({
  separator: { height: StyleSheet.hairlineWidth, marginLeft: Spacing.three },
  footer: { padding: Spacing.three, gap: Spacing.two },
  button: { borderWidth: 1.5, borderRadius: 10, paddingVertical: Spacing.three - 2, alignItems: 'center' },
  buttonText: { fontSize: 16, fontWeight: '600' },
  hint: { fontSize: 13, lineHeight: 18, textAlign: 'center' },
});
