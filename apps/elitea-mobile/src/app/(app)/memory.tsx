import { useState } from 'react';
import { ActivityIndicator, Alert, FlatList, Pressable, StyleSheet, Text, View } from 'react-native';

import { speaksContract } from '@/api/discovery';
import {
  clearMemories,
  deleteMemory,
  memoryErrorText,
  useForgetMemories,
  useMemories,
} from '@/api/queries/memories';
import { SwipeToDelete } from '@/components/SwipeToDelete';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { useShareOut } from '@/security/policy';
import { getSession, useSession } from '@/state/session';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/**
 * Settings > Memory: what the workspace remembers about the caller in the
 * selected project (client contract 1.3). Swipe a row to delete it; "Clear
 * all" deletes every one after a confirmation. Online only: nothing here is
 * kept on the device.
 */
export default function MemoryScreen() {
  const palette = usePalette();
  const projectId = useSession((s) => s.projectId);
  const workspace = useWorkspaceInfo();
  const supported = speaksContract(workspace?.discovery, 1, 3);
  const memories = useMemories(projectId, supported);
  const forget = useForgetMemories(projectId);
  const selectable = useShareOut();
  const [note, setNote] = useState<string | null>(null);
  const rows = memories.data ?? [];

  if (!supported) {
    return (
      <View style={[styles.flex, styles.center, { backgroundColor: palette.background }]}>
        <Text style={[styles.empty, { color: palette.textSecondary }]}>This workspace does not offer memory to mobile yet.</Text>
      </View>
    );
  }

  const onDelete = async (id: string): Promise<boolean> => {
    if (projectId === null) return false;
    try {
      await deleteMemory(projectId, id);
      await forget([id]);
      setNote(null);
      return true;
    } catch (error) {
      setNote(memoryErrorText(error, 'delete'));
      return false;
    }
  };

  const onClearAll = () => {
    // The native Alert outlives the screen: a workspace or project switch can
    // complete while it is open (a connect link confirmed on top of it). The
    // request goes to whichever workspace is active at tap time, so a stale
    // confirmation must clear nothing rather than this project id over there.
    const offeredFor = { workspaceId: getSession().workspaceId, projectId };
    Alert.alert('Clear all memories', 'The workspace forgets everything it remembers about you in this project. This cannot be undone.', [
      { text: 'Cancel', style: 'cancel' },
      {
        text: 'Clear all',
        style: 'destructive',
        onPress: () => {
          const now = getSession();
          if (projectId === null || now.workspaceId !== offeredFor.workspaceId || now.projectId !== offeredFor.projectId) return;
          clearMemories(projectId)
            .then(() => forget('all'))
            .then(() => setNote(null))
            .catch((error: unknown) => setNote(memoryErrorText(error, 'clear')));
        },
      },
    ]);
  };

  return (
    <View style={[styles.flex, { backgroundColor: palette.background }]}>
      {note ? (
        <Text accessibilityRole="alert" style={[styles.note, { color: palette.danger }]}>
          {note}
        </Text>
      ) : null}
      {rows.length > 0 ? (
        <Pressable accessibilityRole="button" onPress={onClearAll} style={styles.clear}>
          <Text style={{ color: palette.danger, fontWeight: '600' }}>Clear all</Text>
        </Pressable>
      ) : null}
      {memories.isLoading ? (
        <ActivityIndicator style={styles.loading} color={palette.accent} />
      ) : (
        <FlatList
          data={rows}
          keyExtractor={(m) => m.id}
          onRefresh={() => void memories.refetch()}
          refreshing={memories.isRefetching}
          ListHeaderComponent={
            <Text style={[styles.hint, { color: palette.textSecondary }]}>
              What the workspace recalls before it answers you in this project. Swipe left to delete one.
            </Text>
          }
          ListEmptyComponent={
            <Text style={[styles.empty, { color: palette.textSecondary }]}>
              {memories.error ? memoryErrorText(memories.error) : 'Nothing remembered yet.'}
            </Text>
          }
          ItemSeparatorComponent={() => <View style={[styles.separator, { backgroundColor: palette.border }]} />}
          renderItem={({ item }) => (
            <SwipeToDelete actionLabel="Delete" onDelete={() => onDelete(item.id)} testID={`memory-${item.id}`}>
              {(a11y) => (
                <View
                  {...a11y}
                  style={styles.row}
                  accessible
                  accessibilityLabel={item.enabled ? item.content : `${item.content}, turned off`}>
                  <Text selectable={selectable} style={[styles.content, { color: item.enabled ? palette.text : palette.textSecondary }]}>
                    {item.content}
                  </Text>
                  {item.tags.length > 0 || !item.enabled ? (
                    <Text style={[styles.meta, { color: palette.textSecondary }]}>
                      {[...(item.enabled ? [] : ['Off']), ...item.tags.map((t) => `#${t}`)].join('  ')}
                    </Text>
                  ) : null}
                </View>
              )}
            </SwipeToDelete>
          )}
        />
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  center: { alignItems: 'center', justifyContent: 'center' },
  loading: { padding: Spacing.four },
  hint: { fontSize: 13, paddingHorizontal: Spacing.three, paddingVertical: Spacing.two },
  clear: { paddingHorizontal: Spacing.three, paddingTop: Spacing.two, alignItems: 'flex-end' },
  note: { paddingHorizontal: Spacing.three, paddingTop: Spacing.two, fontSize: 14 },
  empty: { textAlign: 'center', padding: Spacing.four, fontSize: 15 },
  separator: { height: StyleSheet.hairlineWidth, marginLeft: Spacing.three },
  row: { padding: Spacing.three, gap: Spacing.one },
  content: { fontSize: 15, lineHeight: 21 },
  meta: { fontSize: 13 },
});
