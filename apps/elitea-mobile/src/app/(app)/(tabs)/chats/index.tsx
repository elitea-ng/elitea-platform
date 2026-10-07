import { useQueryClient } from '@tanstack/react-query';
import { router, Stack } from 'expo-router';
import { useMemo, useState } from 'react';
import { ActivityIndicator, Alert, RefreshControl, StyleSheet, View } from 'react-native';

import { deleteConversation, flattenConversationPages, useConversations } from '@/api/queries/conversations';
import { queryKeys } from '@/api/queries/keys';
import { OfflineNotice } from '@/components/OfflineNotice';
import { StatusScreen } from '@/components/StatusScreen';
import { useDesign } from '@/design/DesignProvider';
import { ConversationList, type ChatListItem } from '@/features/chats/ConversationList';
import { filterConversations } from '@/features/chats/filterConversations';
import { isOfflineError, OFFLINE_EMPTY } from '@/features/chats/offlineEmpty';
import { useProjectSelection } from '@/features/projects/useProjectSelection';
import { WorkspacePill } from '@/features/workspaces/WorkspacePill';
import { formatAge } from '@/lib/time';
import { Button, EmptyState, haptic, searchBarOptions } from '@/ui';

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
  const queryClient = useQueryClient();

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
    // No connection and nothing saved on the device yet (§4.9 "offline with no cache"): say so,
    // with the mascot (no data was lost), instead of a generic failure.
    if (isOfflineError(conversations.error)) {
      return (
        <StatusScreen mascot="bust" title={OFFLINE_EMPTY.title} body={OFFLINE_EMPTY.body}>
          <Button title="Try again" size="small" variant="tonal" onPress={() => void conversations.refetch()} />
        </StatusScreen>
      );
    }
    return (
      <StatusScreen title="Couldn't load conversations" body="Check your connection and try again.">
        <Button title="Try again" size="small" variant="tonal" onPress={() => void conversations.refetch()} />
      </StatusScreen>
    );
  }

  const confirmDelete = (row: ChatListItem): Promise<boolean> =>
    new Promise((resolve) => {
      const projectId = selection.projectId;
      if (projectId === null) {
        resolve(false);
        return;
      }
      Alert.alert(
        `Delete “${row.name || 'Untitled conversation'}”?`,
        'The chat and its messages are deleted for you in this workspace. This cannot be undone.',
        [
          { text: 'Cancel', style: 'cancel', onPress: () => resolve(false) },
          {
            text: 'Delete',
            style: 'destructive',
            onPress: () => {
              void deleteConversation(projectId, row.id).then((ok) => {
                if (ok) {
                  haptic('swipeActionThreshold');
                  void queryClient.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
                } else {
                  Alert.alert("Couldn't delete the chat", 'Check your connection and try again.');
                }
                resolve(ok);
              });
            },
          },
        ],
        { cancelable: true, onDismiss: () => resolve(false) },
      );
    });

  return (
    <>
      {header}
      <ConversationList
        rows={rows.map((c) => ({ id: c.id, name: c.name, updated_at: c.updated_at, age: formatAge(c.updated_at) }))}
        onOpen={(row) => router.push({ pathname: '/c/[id]', params: { id: String(row.id) } })}
        // The offline copy cannot be changed: no swipe there.
        onDelete={offline ? null : confirmDelete}
        refreshControl={
          <RefreshControl
            refreshing={conversations.isRefetching && !conversations.isFetchingNextPage}
            onRefresh={() => void conversations.refetch()}
            tintColor={colors.accent}
          />
        }
        header={
          <>
            <WorkspacePill />
            <OfflineNotice visible={offline} />
          </>
        }
        onEndReached={() => {
          if (conversations.hasNextPage && !conversations.isFetchingNextPage) {
            void conversations.fetchNextPage();
          }
        }}
        empty={
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
        footer={conversations.isFetchingNextPage ? <ActivityIndicator style={styles.footer} color={colors.accent} /> : undefined}
      />
    </>
  );
}

const styles = StyleSheet.create({
  center: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  footer: { padding: 16 },
});
