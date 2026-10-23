import { router, Stack, useIsFocused } from 'expo-router';
import { useEffect, useMemo, useRef, useState } from 'react';
import { ActivityIndicator, Alert, RefreshControl, StyleSheet, View } from 'react-native';
import { SafeAreaView } from 'react-native-safe-area-context';

import { speaksContract } from '@/api/discovery';
import { flattenConversationPages, useConversations, useLocalWorkConversations } from '@/api/queries/conversations';
import { FORK_BUSY_TEXT, forkStillInScope, runFork } from '@/api/queries/forkConversation';
import { queryClient } from '@/api/queryClient';
import { OfflineNotice } from '@/components/OfflineNotice';
import { StatusScreen } from '@/components/StatusScreen';
import { useDesign } from '@/design/DesignProvider';
import { ACTION_REFUSED_BODY, ACTION_RETRY_BODY, actionFailedTitle } from '@/features/chats/actionCopy';
import { buildChatRows, buildSearchRows } from '@/features/chats/chatListModel';
import { ThreadView } from '@/features/chat/ThreadView';
import { ConversationList, type ChatListItem, type ChatRowActions } from '@/features/chats/ConversationList';
import { UNTITLED } from '@/features/chats/ConversationRow';
import { isOfflineError, OFFLINE_EMPTY } from '@/features/chats/offlineEmpty';
import { UndoToast } from '@/features/chats/UndoToast';
import {
  useArchiveAvailable,
  useArchivedListAvailable,
  useConversationSearch,
  useListActionsAvailable,
  useSavedPinnedRows,
} from '@/features/chats/useChatListData';
import { usePendingDelete } from '@/features/chats/usePendingDelete';
import { clearSelectedChat, selectChat, useSelectedChat } from '@/features/chats/selectedChat';
import { isTwoPane, listPaneWidth, useScreenWidth } from '@/features/chats/twoPane';
import { HANDOFF_CONTRACT } from '@/features/localWork/model';
import { useProjectSelection } from '@/features/projects/useProjectSelection';
import { WorkspacePill } from '@/features/workspaces/WorkspacePill';
import { log } from '@/lib/log';
import { useCoarseClock } from '@/lib/useCoarseClock';
import { getSession, useSession } from '@/state/session';
import { useWorkspaceInfo } from '@/state/workspaceInfo';
import { runConversationAction, subscribeActionFailures, useConversationOverlay } from '@/sync/appConversationOps';
import { Button, EmptyState, haptic, ListRow, ListSection, searchBarOptions, SegmentedControl } from '@/ui';

interface DeleteScope {
  workspaceId: string | null;
  projectId: number | null;
}
const sameDeleteScope = (a: DeleteScope, b: DeleteScope) => a.workspaceId === b.workspaceId && a.projectId === b.projectId;

/**
 * The signed-in user's conversations in the selected project: Pinned on top, then by day;
 * rename, pin, archive (client contract 1.10, with a link to Archived chats) and delete (swipe, long press, accessibility actions; queued on the device and
 * sent when the workspace can take them); and full-text search over the titles and messages
 * kept on the device, which works offline (src/db/search.ts).
 *
 * At regular width (iPad full screen, 2/3 Split View, a wide Stage Manager window; issue #15)
 * the list and the selected chat sit side by side: a tap opens the chat in the detail column.
 * Below it, the phone stack: a tap pushes the chat's own screen. The selection is kept across
 * a change of width: collapsing with a chat open pushes that chat, and a chat opened on its own
 * screen is the one selected when the two panes come back. The lock screen and the privacy
 * cover sit above both columns, like any screen.
 */
export default function ConversationsScreen() {
  const design = useDesign();
  const { colors } = design;
  const selection = useProjectSelection();
  const projectId = selection.projectId;
  const chats = useConversations(projectId);
  // The Local work filter (desktop agent threads, `source=local_work`): its own list, cache and
  // cursor; offered from the server version that has the cloud handoff (client contract 1.8).
  const workspace = useWorkspaceInfo();
  const localWorkOffered = speaksContract(workspace?.discovery, HANDOFF_CONTRACT.major, HANDOFF_CONTRACT.minor);
  const [filter, setFilter] = useState<'chats' | 'local_work'>('chats');
  const localWorkOnly = localWorkOffered && filter === 'local_work';
  const localWork = useLocalWorkConversations(projectId, localWorkOnly);
  const conversations = localWorkOnly ? localWork : chats;
  const all = flattenConversationPages(conversations.data?.pages);
  const offline = conversations.data?.pages.some((page) => page.offline === true) ?? false;
  const [query, setQuery] = useState('');
  const searching = query.trim() !== '';
  const actionsAvailable = useListActionsAvailable();
  const archivedListAvailable = useArchivedListAvailable();
  const archiveAvailable = useArchiveAvailable(projectId);
  // Fork (client contract 1.11): the whole history into a new chat, opened at once.
  const forkAvailable = speaksContract(workspace?.discovery, 1, 11);
  // The row being forked (busy until the new chat opens); one fork at a time.
  const [forkingId, setForkingId] = useState<number | null>(null);
  const overlay = useConversationOverlay(projectId);
  const savedPinned = useSavedPinnedRows(projectId, actionsAvailable);
  const hits = useConversationSearch(projectId, query, localWorkOnly ? 'local_work' : 'chat');
  const workspaceId = useSession((session) => session.workspaceId);
  const deleteScope = useMemo(() => ({ workspaceId, projectId }), [workspaceId, projectId]);
  const screen = useScreenWidth();
  const twoPane = isTwoPane(screen.width);
  const selectedId = useSelectedChat(deleteScope);
  const focused = useIsFocused();
  // Ages, date sections and the activity checks (a finished turn's lasts a day) move on with it.
  const now = useCoarseClock(focused);
  // Two panes collapsing into the phone stack with a chat open (rotation, Split View, Stage
  // Manager): that chat is pushed, so it stays open. Only a change seen after the first layout
  // counts: the window's width is a guess until the screen has measured its own.
  const wasTwoPane = useRef<boolean | null>(null);
  useEffect(() => {
    if (!screen.measured) return;
    const was = wasTwoPane.current;
    wasTwoPane.current = twoPane;
    if (was === true && !twoPane && focused && selectedId !== null) {
      router.push({ pathname: '/c/[id]', params: { id: String(selectedId) } });
    }
  }, [screen.measured, twoPane, focused, selectedId]);
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
      if (result === 'failed') Alert.alert(actionFailedTitle({ kind: 'delete' }), ACTION_RETRY_BODY);
    });
  }, sameDeleteScope);
  const hidden = useMemo(
    () => new Set(removal.waiting.filter((item) => item.scope.projectId === projectId).map((item) => item.row.id)),
    [removal.waiting, projectId],
  );
  const rows = useMemo(
    () =>
      searching
        ? buildSearchRows({ hits, loaded: all, query, overlay, hidden, now, localWork: localWorkOnly })
        : buildChatRows({ loaded: all, savedPinned: actionsAvailable && !localWorkOnly ? savedPinned : [], overlay, hidden, now, localWork: localWorkOnly }),
    [searching, hits, all, query, overlay, hidden, savedPinned, actionsAvailable, localWorkOnly, now],
  );

  // A queued action the workspace refused: the list goes back to what it says; say why. A screen
  // pushed over this one (Archived chats) is asked first and may take it (src/sync/actionFailures.ts).
  useEffect(
    () =>
      subscribeActionFailures((op) => {
        if (op.projectId !== projectId) return false;
        Alert.alert(actionFailedTitle(op.action), ACTION_REFUSED_BODY);
        return true;
      }),
    [projectId],
  );

  // The native search field lives in the navigation bar (iOS UISearchController, Android
  // header search view). It searches the titles and messages kept on the device (the
  // encrypted store's full-text index), so it works offline too.
  // Two panes share one navigation bar: an inline title there, so the bar keeps one height and
  // the detail column (which starts under it) does not move while the list scrolls.
  const header = (
    <Stack.Screen options={{ ...searchBarOptions(design, { placeholder: 'Search chats', onChange: setQuery }), headerLargeTitle: !twoPane }} />
  );

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
    // with the Elitea mark (no data was lost), instead of a generic failure.
    if (isOfflineError(conversations.error)) {
      return (
        <StatusScreen mark="small" title={OFFLINE_EMPTY.title} body={OFFLINE_EMPTY.body}>
          <Button title="Try again" size="small" variant="tonal" onPress={() => void conversations.refetch()} />
        {localWorkOnly ? <Button title="Show chats" size="small" variant="plain" onPress={() => setFilter('chats')} /> : null}
        </StatusScreen>
      );
    }
    return (
      <StatusScreen title="Couldn't load conversations" body="Check your connection and try again.">
        <Button title="Try again" size="small" variant="tonal" onPress={() => void conversations.refetch()} />
        {localWorkOnly ? <Button title="Show chats" size="small" variant="plain" onPress={() => setFilter('chats')} /> : null}
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
              // The open chat is going: the detail column closes with it.
              clearSelectedChat(deleteScope, row.id);
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
    // Pin and rename are the Chats list's; the Local work list offers Delete only.
    onTogglePin:
      actionsAvailable && !localWorkOnly && projectId !== null
        ? (row) => {
            haptic('tabOrChipChange');
            const action = { kind: 'pin', pinned: row.pinned !== true } as const;
            void runConversationAction(projectId, row.id, action).then((result) => {
              if (result === 'failed') Alert.alert(actionFailedTitle(action), ACTION_RETRY_BODY);
            });
          }
        : null,
    onRename: actionsAvailable && !localWorkOnly ? (row) => router.push({ pathname: '/rename', params: { id: String(row.id), name: row.name ?? '' } }) : null,
    // Archive (client contract 1.10): queued like pin, so it works offline; the row leaves at once.
    onToggleArchive:
      archiveAvailable && !localWorkOnly && projectId !== null
        ? (row) => {
            haptic('tabOrChipChange');
            const action = { kind: 'archive', archived: true } as const;
            void runConversationAction(projectId, row.id, action).then((result) => {
              if (result === 'failed') Alert.alert(actionFailedTitle(action), ACTION_RETRY_BODY);
            });
          }
        : null,
    onFork:
      forkAvailable && !localWorkOnly && projectId !== null
        ? (row) => {
            if (forkingId !== null) return;
            setForkingId(row.id);
            // The new chat opens only in the workspace and project the fork started in: ids are
            // per project, so after a switch the same id is another chat.
            const scope = deleteScope;
            void runFork(queryClient, projectId, row.id)
              .then((result) => {
                if (!result.ok) Alert.alert("Couldn't fork the chat", result.message);
                else if (forkStillInScope(scope)) open({ id: result.row.id });
                else log.info('fork done after a scope switch; not opened', { conversationId: result.row.id });
              })
              .finally(() => setForkingId(null));
          }
        : null,
  };

  function open(row: Pick<ChatListItem, 'id'>) {
    if (twoPane) selectChat(deleteScope, row.id);
    else router.push({ pathname: '/c/[id]', params: { id: String(row.id) } });
  }

  const list = (
    <>
      <ConversationList
        rows={rows}
        now={now}
        results={searching}
        onOpen={open}
        selectedId={twoPane ? selectedId : null}
        opensInPlace={twoPane}
        onLayout={twoPane ? undefined : screen.onLayout}
        busyId={forkingId}
        busyText={FORK_BUSY_TEXT}
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
            {localWorkOffered ? (
              <View style={styles.filter}>
                <SegmentedControl
                  accessibilityLabel="Show"
                  value={filter}
                  onChange={setFilter}
                  segments={[
                    { value: 'chats', label: 'Chats' },
                    { value: 'local_work', label: 'Local work' },
                  ]}
                />
              </View>
            ) : null}
            <OfflineNotice visible={offline} />
            {archivedListAvailable && !localWorkOnly && !searching ? (
              <ListSection style={styles.archivedLink}>
                <ListRow title="Archived chats" icon="archive" onPress={() => router.push('/archived')} testID="archived-chats-link" />
              </ListSection>
            ) : null}
          </>
        }
        onEndReached={() => {
          if (conversations.hasNextPage && !conversations.isFetchingNextPage) {
            void conversations.fetchNextPage();
          }
        }}
        empty={
          searching ? (
            <EmptyState mark="small" title={`Nothing found for “${query.trim()}”`} body="Try another word from the chat's title or its messages." />
          ) : localWorkOnly ? (
            <EmptyState
              title="No Local work yet"
              body="Threads you start in the Elitea desktop app appear here. Send one to the cloud to continue it from your phone."
            />
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

  if (!twoPane) {
    return (
      <>
        {header}
        {list}
      </>
    );
  }
  return (
    <>
      {header}
      <View testID="chats-two-pane" style={[styles.row, { backgroundColor: colors.background }]} onLayout={screen.onLayout}>
        <View
          testID="chats-list-pane"
          style={[styles.listPane, { width: listPaneWidth(screen.width), borderEndColor: colors.separator }]}>
          {list}
        </View>
        {/* The detail column starts under the (translucent) navigation bar: its safe area. */}
        <SafeAreaView edges={['top']} style={styles.flex} testID="chats-detail-pane">
          {selectedId !== null ? (
            <ThreadView key={`${workspaceId ?? ''}:${projectId}:${selectedId}`} id={String(selectedId)} chrome="pane" inFront={focused} />
          ) : (
            <EmptyState
              mark="none"
              title="No chat selected"
              body="Choose a chat from the list, or start a new one."
              action={{ title: 'New chat', onPress: () => router.push('/new') }}
            />
          )}
        </SafeAreaView>
      </View>
    </>
  );
}

const styles = StyleSheet.create({
  center: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  footer: { padding: 16 },
  filter: { paddingHorizontal: 16, paddingBottom: 8 },
  archivedLink: { marginHorizontal: 16, marginBottom: 12 },
  flex: { flex: 1 },
  row: { flex: 1, flexDirection: 'row' },
  listPane: { borderEndWidth: StyleSheet.hairlineWidth },
});
