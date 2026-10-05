import { Stack, useLocalSearchParams } from 'expo-router';
import { useEffect, useMemo } from 'react';
import {
  ActivityIndicator,
  FlatList,
  KeyboardAvoidingView,
  Platform,
  Pressable,
  StyleSheet,
  Text,
  View,
} from 'react-native';

import { isConversationRef, useConversation } from '@/api/queries/conversation';
import { useModels } from '@/api/queries/models';
import type { TurnModel } from '@/api/queries/sendMessage';
import { OfflineNotice } from '@/components/OfflineNotice';
import { StatusScreen } from '@/components/StatusScreen';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { Composer } from '@/features/chat/Composer';
import { MessageBubble } from '@/features/chat/MessageBubble';
import { chatTarget, toChatMessages, type ChatMessage } from '@/features/chat/messageModel';
import type { LiveTurn } from '@/features/chat/turnController';
import { useTurn } from '@/features/chat/useTurn';
import { Markdown } from '@/features/markdown/Markdown';
import { useSession } from '@/state/session';
import { outboxDrainer, useOutboxItems } from '@/sync/appOutbox';
import { isOfflineCopy } from '@/sync/offlineReads';
import { outboxStatus, type OutboxItem } from '@/sync/outbox';

type Row =
  | { kind: 'persisted'; key: string; message: ChatMessage }
  | { kind: 'outbox'; key: string; item: OutboxItem }
  | { kind: 'live_user'; key: string; text: string }
  | { kind: 'live_reply'; key: string; live: LiveTurn };

/** One conversation: its latest messages, newest at the bottom, and the composer. */
export default function ThreadScreen() {
  const palette = usePalette();
  const { id } = useLocalSearchParams<{ id: string }>();
  const ref = typeof id === 'string' ? id : '';
  const projectId = useSession((s) => s.projectId);
  const conversation = useConversation(projectId, ref);
  const detail = conversation.data;
  const messages = toChatMessages(detail);
  const target = chatTarget(detail);
  // A model conversation with no saved model runs on the project's default.
  const needsDefaultModel = target.ok && target.kind === 'model' && target.model === null;
  const models = useModels(projectId, needsDefaultModel);
  const savedName = target.ok && target.kind === 'model' ? target.model?.name : undefined;
  const savedProject = target.ok && target.kind === 'model' ? target.model?.projectId : undefined;
  const fallback = models.data?.defaultModel ?? null;
  const model = useMemo<TurnModel | null>(() => {
    if (savedName !== undefined) return { name: savedName, projectId: savedProject ?? null };
    return needsDefaultModel && fallback ? { name: fallback.name, projectId: fallback.projectId } : null;
  }, [savedName, savedProject, needsDefaultModel, fallback]);
  const conversationUuid = typeof detail?.uuid === 'string' && detail.uuid !== '' ? detail.uuid : null;
  const addressable = target.ok && (target.kind === 'agent' || model !== null);
  const turn = useTurn({
    projectId,
    conversationRef: ref,
    conversationUuid,
    participantId: target.ok && addressable ? target.participantId : null,
    model: target.ok && target.kind === 'model' ? model : null,
  });
  const { live, dismiss } = turn;
  const outbox = useOutboxItems(projectId, conversationUuid);

  // Once the persisted copy is back from the server, the live bubbles go.
  useEffect(() => {
    if (live?.settled) dismiss();
  }, [live?.settled, dismiss]);

  if (!isConversationRef(ref)) return <StatusScreen title="Conversation not found" />;
  if (conversation.isLoading) {
    return (
      <View style={[styles.center, { backgroundColor: palette.background }]}>
        <ActivityIndicator color={palette.accent} />
      </View>
    );
  }
  if (conversation.error && !detail) {
    return (
      <StatusScreen title="Couldn't open this conversation" body="Check your connection and try again.">
        <Pressable accessibilityRole="button" onPress={() => void conversation.refetch()}>
          <Text style={[styles.action, { color: palette.accent }]}>Retry</Text>
        </Pressable>
      </StatusScreen>
    );
  }

  const rows: Row[] = [];
  // Inverted list: the first rows are the newest, at the bottom.
  for (const item of [...outbox].reverse()) rows.push({ kind: 'outbox', key: `outbox-${item.questionId}`, item });
  if (live && !live.settled) {
    rows.push({ kind: 'live_reply', key: `live-reply-${live.questionId}`, live });
    const lastUser = [...messages].reverse().find((m) => m.role === 'user');
    if (lastUser?.text !== live.userText) {
      rows.push({ kind: 'live_user', key: `live-user-${live.questionId}`, text: live.userText });
    }
  }
  for (const message of [...messages].reverse()) rows.push({ kind: 'persisted', key: message.key, message });

  const disabledReason = !conversationUuid
    ? 'This conversation cannot be messaged from mobile.'
    : !target.ok
      ? target.reason === 'several'
        ? 'This conversation has several agents or models. Continue it in the web app.'
        : 'This conversation has no agent or model you can message from mobile.'
      : addressable
        ? null
        : models.isLoading
          ? 'Loading the workspace model…'
          : 'This project has no model to chat with.';

  return (
    <KeyboardAvoidingView
      style={[styles.flex, { backgroundColor: palette.background }]}
      behavior={Platform.OS === 'ios' ? 'padding' : undefined}
      keyboardVerticalOffset={Platform.OS === 'ios' ? 100 : 0}>
      <Stack.Screen options={{ title: detail?.name || 'Conversation' }} />
      <OfflineNotice visible={isOfflineCopy(detail)} />
      <FlatList
        inverted
        data={rows}
        keyExtractor={(row) => row.key}
        contentContainerStyle={styles.list}
        keyboardShouldPersistTaps="handled"
        renderItem={({ item }) => {
          switch (item.kind) {
            case 'persisted':
              return <PersistedMessage message={item.message} />;
            case 'live_user':
              return <MessageBubble role="user">{item.text}</MessageBubble>;
            case 'outbox':
              return <QueuedMessage item={item.item} onSendAgain={turn.send} />;
            case 'live_reply':
              return (
                <LiveReply
                  live={item.live}
                  author={target.ok ? target.name : null}
                  onRetry={turn.retry}
                  onReconnect={turn.reconnect}
                  onDismiss={turn.dismiss}
                />
              );
          }
        }}
        ListEmptyComponent={
          <Text style={[styles.empty, { color: palette.textSecondary }]}>No messages yet.</Text>
        }
      />
      <Composer disabledReason={disabledReason} busy={turn.busy} onSend={turn.send} />
    </KeyboardAvoidingView>
  );
}

function PersistedMessage({ message }: { message: ChatMessage }) {
  const status = message.isStreaming
    ? 'Still generating…'
    : message.hasUnsupportedItems
      ? 'Part of this message can only be viewed in the web app.'
      : null;
  return (
    <MessageBubble role={message.role} author={message.author} status={status}>
      {message.role === 'assistant' && message.text !== '' ? <Markdown value={message.text} /> : message.text}
    </MessageBubble>
  );
}

/** A message waiting in the offline outbox (queued, sending, or refused). */
function QueuedMessage({ item, onSendAgain }: { item: OutboxItem; onSendAgain: (text: string) => Promise<string | null> }) {
  const palette = usePalette();
  const status = outboxStatus(item);
  const actions =
    item.state === 'failed'
      ? [
          // A new message with a new question_id; the refused one is dropped
          // only once the new one is accepted, so a failed resend loses nothing.
          {
            label: 'Send again',
            onPress: () =>
              void onSendAgain(item.userText).then((problem) => {
                if (problem === null) return outboxDrainer.remove(item.questionId);
              }),
          },
          { label: 'Delete', onPress: () => void outboxDrainer.remove(item.questionId) },
        ]
      : item.state === 'queued'
        ? [
            { label: 'Retry now', onPress: () => void outboxDrainer.retryNow(item.questionId) },
            { label: 'Delete', onPress: () => void outboxDrainer.remove(item.questionId) },
          ]
        : [];
  return (
    <MessageBubble
      role="user"
      status={status.error ? null : status.text}
      error={status.error ? status.text : null}
      footer={
        actions.length > 0 ? (
          <View style={styles.actions}>
            {actions.map((a) => (
              <Pressable key={a.label} accessibilityRole="button" onPress={a.onPress}>
                <Text style={[styles.actionSmall, { color: palette.accent }]}>{a.label}</Text>
              </Pressable>
            ))}
          </View>
        ) : null
      }>
      {item.userText}
    </MessageBubble>
  );
}

function LiveReply({
  live,
  author,
  onRetry,
  onReconnect,
  onDismiss,
}: {
  live: LiveTurn;
  author: string | null;
  onRetry: () => void;
  onReconnect: () => void;
  onDismiss: () => void;
}) {
  const palette = usePalette();
  const { turn } = live;
  const waiting = turn.phase === 'sending' || (turn.phase === 'streaming' && turn.content === '');
  const status =
    turn.status ??
    (turn.phase === 'sending' ? 'Sending…' : waiting ? 'Thinking…' : live.streamStatus === 'retrying' ? 'Reconnecting…' : null);
  const actions = [
    live.canReconnect ? { label: 'Reconnect', onPress: onReconnect } : null,
    turn.phase === 'failed' && turn.error?.retryable ? { label: 'Retry', onPress: onRetry } : null,
    turn.phase === 'failed' ? { label: 'Dismiss', onPress: onDismiss } : null,
  ].filter((a): a is { label: string; onPress: () => void } => a !== null);
  return (
    <MessageBubble
      role="assistant"
      author={author}
      status={status}
      error={turn.phase === 'failed' ? (turn.error?.message ?? null) : null}
      footer={
        actions.length > 0 ? (
          <View style={styles.actions}>
            {actions.map((a) => (
              <Pressable key={a.label} accessibilityRole="button" onPress={a.onPress}>
                <Text style={[styles.actionSmall, { color: palette.accent }]}>{a.label}</Text>
              </Pressable>
            ))}
          </View>
        ) : null
      }>
      {turn.content !== '' ? <Markdown value={turn.content} streaming={turn.phase === 'streaming'} /> : ''}
    </MessageBubble>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  center: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  list: { paddingVertical: Spacing.two },
  // The list is inverted, so flip the empty state back upright.
  empty: { textAlign: 'center', padding: Spacing.four, fontSize: 15, transform: [{ scaleY: -1 }] },
  action: { fontSize: 16, fontWeight: '600' },
  actions: { flexDirection: 'row', gap: Spacing.three, marginTop: Spacing.one },
  actionSmall: { fontSize: 14, fontWeight: '600' },
});
