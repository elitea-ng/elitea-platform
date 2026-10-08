import { router, Stack } from 'expo-router';
import { useEffect, useMemo, useState } from 'react';
import { ActivityIndicator, Alert, RefreshControl, StyleSheet, View } from 'react-native';

import { flattenConversationPages, useConversations } from '@/api/queries/conversations';
import { OfflineNotice } from '@/components/OfflineNotice';
import { StatusScreen } from '@/components/StatusScreen';
import { useDesign } from '@/design/DesignProvider';
import { buildChatRows, buildSearchRows } from '@/features/chats/chatListModel';
import { ConversationList, type ChatListItem, type ChatRowActions } from '@/features/chats/ConversationList';
import { UNTITLED } from '@/features/chats/ConversationRow';
import { isOfflineError, OFFLINE_EMPTY } from '@/features/chats/offlineEmpty';
import { UndoToast } from '@/features/chats/UndoToast';
import { useConversationSearch, useListActionsAvailable, useSavedPinnedRows } from '@/features/chats/useChatListData';
import { usePendingDelete } from '@/features/chats/usePendingDelete';
import { useProjectSelection } from '@/features/projects/useProjectSelection';
import { WorkspacePill } from '@/features/workspaces/WorkspacePill';
import { log } from '@/lib/log';
import { getSession, useSession } from '@/state/session';
import { runConversationAction, subscribeActionFailures, useConversationOverlay } from '@/sync/appConversationOps';
import { Button, EmptyState, haptic, searchBarOptions } from '@/ui';

interface DeleteScope {
  workspaceId: string | null;
  projectId: number | null;
}
const sameDeleteScope = (a: DeleteScope, b: DeleteScope) => a.workspaceId === b.workspaceId && a.projectId === b.projectId;

const FAILED_COPY = {
  rename: "Couldn't rename the chat",
  pin: "Couldn't change the pin",
  delete: "Couldn't delete the chat",
} as const;

/**
 * The signed-in user's conversations in the selected project: Pinned on top, then by day;
 * rename, pin and delete (swipe, long press, accessibility actions; queued on the device and
 * sent when the workspace can take them); and full-text search over the titles and messages
 * kept on the device, which works offline (src/db/search.ts).
 */
export default function ConversationsScreen() {
  const design = useDesign();
  const { colors } = design;
  const selection = useProjectSelection();
  const projectId = selection.projectId;
  const conversations = useConversations(projectId);
  const all = flattenConversationPages(conversations.data?.pages);
  const offline = conversations.data?.pages.some((page) => page.offline === true) ?? false;
  const [query, setQuery] = useState('');
  const searching = query.trim() !== '';
  const actionsAvailable = useListActionsAvailable();
  const overlay = useConversationOverlay(projectId);
  const savedPinned = useSavedPinnedRows(projectId, actionsAvailable);
  const hits = useConversationSearch(projectId, query);
  const workspaceId = useSession((session) => session.workspaceId);
  const deleteScope = useMemo(() => ({ workspaceId, projectId }), [workspaceId, projectId]);
  // The delete goes to the project (and workspace) the row was in when it was confirmed:
  // conversation ids are per project, so the same id elsewhere is another chat.
  const removal = usePendingDelete<ChatListItem, DeleteScope>(deleteScope, (row, scope) => {
    if (scope.projectId === null) return;
    if (getSession().workspaceId !== scope.workspaceId) {
      // Switched workspace (or signed out) inside the window: the session and store are
      // another workspace's now, so the id means nothing there. Never send it.
      log.warn('pending delete dropped: workspace changed', { conversationId: row.id });
      return;
    }
    // Returned, so the row stays hidden until the list shows the delete (queued) or it is sent.
    return runConversationAction(scope.projectId, row.id, { kind: 'delete' }).then((result) => {
      if (result === 'failed') Alert.alert(FAILED_COPY.delete, 'Check your connection and try again.');
    });
  }, sameDeleteScope);
  const hidden = useMemo(
    () => new Set(removal.waiting.filter((item) => item.scope.projectId === projectId).map((item) => item.row.id)),
    [removal.waiting, projectId],
  );
  const rows = useMemo(
    () =>
      searching
        ? buildSearchRows({ hits, loaded: all, query, overlay, hidden })
        : buildChatRows({ loaded: all, savedPinned: actionsAvailable ? savedPinned : [], overlay, hidden }),
    [searching, hits, all, query, overlay, hidden, savedPinned, actionsAvailable],
  );

  // A queued action the workspace refused: the list goes back to what it says; say why.
  useEffect(
    () =>
      subscribeActionFailures((op) => {
        if (op.projectId === projectId) Alert.alert(FAILED_COPY[op.action.kind], 'The workspace did not accept the change.');
      }),
    [projectId],
  );

  // The native search field lives in the navigation bar (iOS UISearchController, Android
  // header search view). It searches the titles and messages kept on the device (the
  // encrypted store's full-text index), so it works offline too.
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
      if (projectId === null) {
        resolve(false);
        return;
      }
      // Delete always asks first (§4.11), then waits out a short Undo before it is sent.
      Alert.alert(
        `Delete “${row.name || UNTITLED}”?`,
        'The chat and its messages are deleted in this workspace, on every device.',
        [
          { text: 'Cancel', style: 'cancel', onPress: () => resolve(false) },
          {
            text: 'Delete',
            style: 'destructive',
            onPress: () => {
              haptic('swipeActionThreshold');
              removal.start(row);
              resolve(true);
            },
          },
        ],
        { cancelable: true, onDismiss: () => resolve(false) },
      );
    });

  const actions: ChatRowActions = {
    onDelete: projectId === null ? null : confirmDelete,
    onTogglePin:
      actionsAvailable && projectId !== null
        ? (row) => {
            haptic('tabOrChipChange');
            void runConversationAction(projectId, row.id, { kind: 'pin', pinned: row.pinned !== true }).then((result) => {
              if (result === 'failed') Alert.alert(FAILED_COPY.pin, 'Check your connection and try again.');
            });
          }
        : null,
    onRename: actionsAvailable ? (row) => router.push({ pathname: '/rename', params: { id: String(row.id), name: row.name ?? '' } }) : null,
  };

  return (
    <>
      {header}
      <ConversationList
        rows={rows}
        results={searching}
        onOpen={(row) => router.push({ pathname: '/c/[id]', params: { id: String(row.id) } })}
        // Offline too: the actions are queued on the device and sent on reconnect.
        actions={actions}
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
            <EmptyState mascot="bust" title={`Nothing found for “${query.trim()}”`} body="Try another word from the chat's title or its messages." />
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
      <UndoToast
        text={removal.pending ? 'Chat deleted' : null}
        subject={removal.pending}
        subjectName={removal.pending ? removal.pending.name || UNTITLED : null}
        onUndo={removal.undo}
      />
    </>
  );
}

const styles = StyleSheet.create({
  center: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  footer: { padding: 16 },
});
