import { ScrollView, StyleSheet } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { WorkspaceRow } from '@/features/workspaces/WorkspaceRow';
import { MAX_WORKSPACES } from '@/security/workspaceRegistry';
import type { WorkspaceSummary } from '@/state/workspaceList';
import { Button, ListSection, Text } from '@/ui';

/**
 * The switcher's content (the `/workspaces` sheet and its `__DEV__` preview draw this): the
 * connected workspaces as one inset-grouped list, the active one first, then "Add workspace".
 */
export function WorkspacesSheet({
  workspaces,
  activeId,
  onOpen,
  onRemove,
  onAdd,
}: {
  workspaces: readonly WorkspaceSummary[];
  activeId: string | null;
  onOpen: (workspace: WorkspaceSummary) => void;
  onRemove: (workspace: WorkspaceSummary) => void;
  onAdd: () => void;
}) {
  const { colors, space } = useDesign();
  const full = workspaces.length >= MAX_WORKSPACES;
  const ordered = [...workspaces].sort((a, b) => Number(b.id === activeId) - Number(a.id === activeId));
  return (
    <ScrollView
      style={{ backgroundColor: colors.elevated }}
      contentInsetAdjustmentBehavior="automatic"
      contentContainerStyle={[styles.content, { paddingHorizontal: space.screenGutterCompact }]}>
      <ListSection title="On this device" footer="Each workspace keeps its own sign-in, data and settings. Switching never deletes anything.">
        {ordered.map((workspace) => (
          <WorkspaceRow
            key={workspace.id}
            workspace={workspace}
            active={workspace.id === activeId}
            onPress={() => onOpen(workspace)}
            onRemove={() => onRemove(workspace)}
          />
        ))}
      </ListSection>
      <Button title="Add workspace" variant="tonal" icon="plus" testID="add-workspace" disabled={full} onPress={onAdd} />
      {full ? (
        <Text variant="footnote" tone="secondary" style={styles.center}>
          At most {MAX_WORKSPACES} workspaces can be connected. Remove one to add another.
        </Text>
      ) : null}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  content: { paddingVertical: 16, gap: 20 },
  center: { textAlign: 'center' },
});
