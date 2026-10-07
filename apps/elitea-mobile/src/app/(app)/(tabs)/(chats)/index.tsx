import { router, Stack } from 'expo-router';
import { useMemo, useState } from 'react';
import { ActivityIndicator, FlatList, RefreshControl, StyleSheet, View } from 'react-native';

import { flattenConversationPages, useConversations } from '@/api/queries/conversations';
import { OfflineNotice } from '@/components/OfflineNotice';
import { StatusScreen } from '@/components/StatusScreen';
import { useDesign } from '@/design/DesignProvider';
import { ConversationRow, ConversationSeparator } from '@/features/chats/ConversationRow';
import { filterConversations } from '@/features/chats/filterConversations';
import { useProjectSelection } from '@/features/projects/useProjectSelection';
import { WorkspacePill } from '@/features/workspaces/WorkspacePill';
import { formatAge } from '@/lib/time';
import { Button, EmptyState, searchBarOptions } from '@/ui';

/** The signed-in user's conversations in the selected project. */
export default function ConversationsScreen() {
  const design = useDesign();
  const { colors } = design;
  const selection = useProjectSelection();
  const conversations = useConversations(selection.projectId);
  const all = flattenConversationPages(conversations.data?.pages);
  const offline = conversations.data?.pages.some((page) => page.offline === true) ?? false;
  const [query, setQuery] = useState('');
  const rows = useMemo(() => filterConversations(all, query), [all, query]);
  const searching = query.trim() !== '';

  // The native search field lives in the navigation bar (iOS UISearchController, Android
  // header search view). It searches the loaded (or offline-cached) titles on the device.
  const header = <Stack.Screen options={searchBarOptions(design, { placeholder: 'Search chats', onChange: setQuery })} />;

  if (selection.noProject) {
    return <StatusScreen title="No active project" body="Your account has no active project in this workspace." />;
  }
  if (selection.projectId === null || conversations.isLoading) {
    return (
      <View style={[styles.center, { backgroundColor: colors.background }]}>
        <ActivityIndicator color={colors.accent} />
      </View>
    );
  }
  if (conversations.error && all.length === 0) {
    return (
      <StatusScreen title="Couldn't load conversations" body="Check your connection and try again.">
        <Button title="Try again" size="small" variant="tonal" onPress={() => void conversations.refetch()} />
      </StatusScreen>
    );
  }

  return (
    <>
      {header}
      <FlatList
        contentInsetAdjustmentBehavior="automatic"
        keyboardDismissMode="on-drag"
        style={{ backgroundColor: colors.background }}
        contentContainerStyle={rows.length === 0 ? styles.emptyContainer : styles.list}
        data={rows}
        keyExtractor={(c) => String(c.id)}
        refreshControl={
          <RefreshControl
            refreshing={conversations.isRefetching && !conversations.isFetchingNextPage}
            onRefresh={() => void conversations.refetch()}
            tintColor={colors.accent}
          />
        }
        ListHeaderComponent={
          <>
            <WorkspacePill />
            <OfflineNotice visible={offline} />
          </>
        }
        onEndReachedThreshold={0.5}
        onEndReached={() => {
          if (conversations.hasNextPage && !conversations.isFetchingNextPage) {
            void conversations.fetchNextPage();
          }
        }}
        ListEmptyComponent={
          searching ? (
            <EmptyState mascot="bust" title={`Nothing found for “${query.trim()}”`} body="Try another word from the chat's title." />
          ) : (
            <EmptyState
              title="Ask me anything"
              body="Your conversations with workspace agents will appear here."
              action={{ title: 'New chat', onPress: () => router.push('/new') }}
            />
          )
        }
        ListFooterComponent={conversations.isFetchingNextPage ? <ActivityIndicator style={styles.footer} color={colors.accent} /> : undefined}
        ItemSeparatorComponent={ConversationSeparator}
        renderItem={({ item }) => (
          <ConversationRow
            title={item.name}
            age={formatAge(item.updated_at)}
            onPress={() => router.push({ pathname: '/c/[id]', params: { id: String(item.id) } })}
          />
        )}
      />
    </>
  );
}

const styles = StyleSheet.create({
  center: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  emptyContainer: { flexGrow: 1 },
  list: { paddingBottom: 24 },
  footer: { padding: 16 },
});
