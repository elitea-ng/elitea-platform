import { router } from 'expo-router';
import { useEffect, useMemo } from 'react';
import { ActivityIndicator, Alert, RefreshControl, StyleSheet, View } from 'react-native';

import { useArchivedConversations } from '@/api/queries/archivedConversations';
import { flattenConversationPages } from '@/api/queries/conversations';
import { StatusScreen } from '@/components/StatusScreen';
import { useDesign } from '@/design/DesignProvider';
import { ACTION_REFUSED_BODY, ACTION_RETRY_BODY, actionFailedTitle } from '@/features/chats/actionCopy';
import { buildArchivedRows } from '@/features/chats/chatListModel';
import { ConversationList, type ChatRowActions } from '@/features/chats/ConversationList';
import { isOfflineError } from '@/features/chats/offlineEmpty';
import { useArchiveAvailable, useArchivedListAvailable } from '@/features/chats/useChatListData';
import { useOnline } from '@/lib/network';
import { useSession } from '@/state/session';
import { runConversationAction, subscribeActionFailures, useConversationOverlay } from '@/sync/appConversationOps';
import { Button, EmptyState, haptic } from '@/ui';

/** The copy of the screen's states, shared with its tests. */
export const ARCHIVED_COPY = {
  offlineTitle: 'Archived chats need a connection',
  offlineBody: 'Archived chats are not kept on this device. Connect to see them.',
  emptyTitle: 'No archived chats',
  emptyBody: 'Chats you archive leave the Chats list and wait here. Anyone in the project who can archive can bring them back.',
  unsupportedTitle: 'Archived chats are not available',
  unsupportedBody: 'This workspace does not support archiving chats yet.',
} as const;

/**
 * Archived chats (client contract 1.10): the project's archived chats, newest first, each with
 * who archived it (`listConversations` with `archived=only`, as the web app's page; the archive
 * is the project's, like the pin). A tap opens the chat (an archived chat still opens
 * and takes new messages); Unarchive (swipe menu, long press, accessibility action; needs the
 * archive permission) is queued like every list action and the row leaves at once. Online only:
 * the offline store keeps the default list, which leaves archived chats out.
 */
export function ArchivedChatsView() {
  const { colors } = useDesign();
  const projectId = useSession((s) => s.projectId);
  const available = useArchivedListAvailable();
  const canUnarchive = useArchiveAvailable(projectId);
  const online = useOnline();
  const archived = useArchivedConversations(projectId, available);
  const overlay = useConversationOverlay(projectId);
  const rows = useMemo(() => buildArchivedRows({ loaded: flattenConversationPages(archived.data?.pages), overlay }), [archived.data, overlay]);

  // A queued unarchive the workspace refused: the row comes back here; say why. This screen
  // takes it (the Chats tab underneath, also listening, does not say it again); any other
  // refusal is left to the Chats tab.
  useEffect(
    () =>
      subscribeActionFailures((op) => {
        if (op.projectId !== projectId || op.action.kind !== 'archive' || op.action.archived) return false;
        Alert.alert(actionFailedTitle(op.action), ACTION_REFUSED_BODY);
        return true;
      }),
    [projectId],
  );

  if (!available) return <StatusScreen title={ARCHIVED_COPY.unsupportedTitle} body={ARCHIVED_COPY.unsupportedBody} />;
  if (projectId === null || archived.isLoading) {
    return (
      <View style={[styles.center, { backgroundColor: colors.background }]}>
        <ActivityIndicator color={colors.accent} />
      </View>
    );
  }
  if (archived.error && rows.length === 0) {
    const offline = !online || isOfflineError(archived.error);
    return (
      <StatusScreen
        mark={offline ? 'small' : undefined}
        title={offline ? ARCHIVED_COPY.offlineTitle : "Couldn't load archived chats"}
        body={offline ? ARCHIVED_COPY.offlineBody : 'Check your connection and try again.'}>
        <Button title="Try again" size="small" variant="tonal" onPress={() => void archived.refetch()} />
      </StatusScreen>
    );
  }

  const actions: ChatRowActions = {
    onDelete: null,
    onToggleArchive: canUnarchive
      ? (row) => {
          haptic('tabOrChipChange');
          const action = { kind: 'archive', archived: false } as const;
          void runConversationAction(projectId, row.id, action).then((result) => {
            if (result === 'failed') Alert.alert(actionFailedTitle(action), ACTION_RETRY_BODY);
          });
        }
      : null,
  };

  return (
    <ConversationList
      rows={rows}
      onOpen={(row) => router.push({ pathname: '/c/[id]', params: { id: String(row.id) } })}
      actions={actions}
      refreshControl={
        <RefreshControl refreshing={archived.isRefetching && !archived.isFetchingNextPage} onRefresh={() => void archived.refetch()} tintColor={colors.accent} />
      }
      onEndReached={() => {
        if (archived.hasNextPage && !archived.isFetchingNextPage) void archived.fetchNextPage();
      }}
      empty={<EmptyState mark="small" title={ARCHIVED_COPY.emptyTitle} body={ARCHIVED_COPY.emptyBody} />}
      footer={archived.isFetchingNextPage ? <ActivityIndicator style={styles.footer} color={colors.accent} /> : undefined}
    />
  );
}

const styles = StyleSheet.create({
  center: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  footer: { padding: 16 },
});
