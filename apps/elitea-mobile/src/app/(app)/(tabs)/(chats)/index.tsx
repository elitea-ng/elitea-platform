import { router } from 'expo-router';
import { ActivityIndicator, FlatList, Pressable, RefreshControl, StyleSheet, Text, View } from 'react-native';

import { flattenConversationPages, useConversations } from '@/api/queries/conversations';
import { OfflineNotice } from '@/components/OfflineNotice';
import { StatusScreen } from '@/components/StatusScreen';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { useProjectSelection } from '@/features/projects/useProjectSelection';
import { formatAge } from '@/lib/time';

/** The signed-in user's conversations in the selected project. */
export default function ConversationsScreen() {
  const palette = usePalette();
  const selection = useProjectSelection();
  const conversations = useConversations(selection.projectId);
  const rows = flattenConversationPages(conversations.data?.pages);
  const offline = conversations.data?.pages.some((page) => page.offline === true) ?? false;

  if (selection.noProject) {
    return (
      <StatusScreen
        title="No active project"
        body="Your account has no active project in this workspace."
      />
    );
  }
  if (selection.projectId === null || conversations.isLoading) {
    return (
      <View style={[styles.center, { backgroundColor: palette.background }]}>
        <ActivityIndicator color={palette.accent} />
      </View>
    );
  }
  if (conversations.error && rows.length === 0) {
    return (
      <StatusScreen title="Couldn't load conversations" body="Check your connection and try again.">
        <Pressable accessibilityRole="button" onPress={() => void conversations.refetch()}>
          <Text style={[styles.action, { color: palette.accent }]}>Retry</Text>
        </Pressable>
      </StatusScreen>
    );
  }

  return (
    <FlatList
      style={{ backgroundColor: palette.background }}
      contentContainerStyle={rows.length === 0 ? styles.emptyContainer : undefined}
      data={rows}
      keyExtractor={(c) => String(c.id)}
      refreshControl={
        <RefreshControl
          refreshing={conversations.isRefetching && !conversations.isFetchingNextPage}
          onRefresh={() => void conversations.refetch()}
          tintColor={palette.accent}
        />
      }
      ListHeaderComponent={<OfflineNotice visible={offline} />}
      onEndReachedThreshold={0.5}
      onEndReached={() => {
        if (conversations.hasNextPage && !conversations.isFetchingNextPage) {
          void conversations.fetchNextPage();
        }
      }}
      ListEmptyComponent={
        <View style={styles.empty}>
          <Text style={[styles.title, { color: palette.text }]}>No conversations yet</Text>
          <Text style={[styles.body, { color: palette.textSecondary }]}>
            Your conversations with workspace agents will appear here.
          </Text>
        </View>
      }
      ListFooterComponent={
        conversations.isFetchingNextPage ? (
          <ActivityIndicator style={styles.footer} color={palette.accent} />
        ) : undefined
      }
      ItemSeparatorComponent={() => <View style={[styles.separator, { backgroundColor: palette.border }]} />}
      renderItem={({ item }) => (
        <Pressable
          accessibilityRole="button"
          onPress={() => router.push({ pathname: '/c/[id]', params: { id: String(item.id) } })}
          style={styles.row}>
          <Text numberOfLines={1} style={[styles.name, { color: palette.text }]}>
            {item.name || 'Untitled conversation'}
          </Text>
          <Text style={[styles.age, { color: palette.textSecondary }]}>{formatAge(item.updated_at)}</Text>
        </Pressable>
      )}
    />
  );
}

const styles = StyleSheet.create({
  center: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  // The Discover entry stays at the top; the empty state takes the rest, centred.
  emptyContainer: { flexGrow: 1 },
  empty: { flex: 1, alignItems: 'center', justifyContent: 'center', padding: Spacing.four, gap: Spacing.two },
  title: { fontSize: 20, fontWeight: '600' },
  body: { fontSize: 15, textAlign: 'center' },
  row: {
    flexDirection: 'row',
    alignItems: 'center',
    paddingHorizontal: Spacing.three,
    paddingVertical: Spacing.three,
    gap: Spacing.two,
  },
  name: { flex: 1, fontSize: 16 },
  age: { fontSize: 13 },
  separator: { height: StyleSheet.hairlineWidth, marginLeft: Spacing.three },
  footer: { padding: Spacing.three },
  action: { fontSize: 16, fontWeight: '600' },
});
