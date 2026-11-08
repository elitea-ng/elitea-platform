import { router } from 'expo-router';
import { useState, type ReactNode } from 'react';
import { Pressable, StyleSheet, View } from 'react-native';

import { restorePreparedTurn, TurnInputError } from '@/api/queries/sendMessage';
import { AttachmentStrip } from '@/features/chat/attachments/AttachmentStrip';
import { MessageBubble } from '@/features/chat/MessageBubble';
import type { ChatAttachment, ChatMessage } from '@/features/chat/messageModel';
import { MESSAGE_CLASS, sideTalkLabel, type MessageClass } from '@elitea/client-core/conversation';
import { useDesign } from '@/design/DesignProvider';
import { TURN_TAKING_COPY } from '@/features/chat/turnTakingCopy';
import { ThinkingDots } from '@/features/chat/ThinkingDots';
import { ToolActivity } from '@/features/chat/ToolCalls';
import type { ToolStep } from '@/features/chat/toolActivity';
import type { LiveTurn } from '@/features/chat/turnController';
import type { SendExtras } from '@/features/chat/useTurn';
import { Markdown } from '@/features/markdown/Markdown';
import { outboxDrainer } from '@/sync/appOutbox';
import { outboxStatus, type OutboxItem } from '@/sync/outbox';
import { Button, Icon, Text } from '@/ui';

/**
 * The rows of a conversation (docs/design-system.md §4.6, screens.md §4), shared by the thread
 * (`/c/[id]`) and its development preview (`/design-chat`), so the preview draws what users see.
 */

/** "Research · 9:41": who answered and when (the time in the device's own format). */
export function messageMeta(author: string | null, createdAt: number | null, now: number = Date.now()): string | null {
  if (author === null && createdAt === null) return null;
  if (createdAt === null) return author;
  const at = new Date(createdAt);
  const sameDay = new Date(now).toDateString() === at.toDateString();
  const time = sameDay
    ? at.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })
    : at.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  return author ? `${author} · ${time}` : time;
}

/**
 * The author line of an answer (ADR-0050 decisions 6 and 7e): the persona's
 * role after the name, and "temporary" for an ephemeral sub-agent or "woke
 * up" for a heartbeat, as the web caption shows them.
 */
export function answerAuthorLine(author: string | null, role: string | null | undefined, messageClass: MessageClass | undefined): string | null {
  const marker = messageClass === MESSAGE_CLASS.Ephemeral ? 'temporary' : messageClass === MESSAGE_CLASS.Heartbeat ? 'woke up' : null;
  const parts = [author, role ?? null, marker].filter((part): part is string => part !== null && part !== '');
  return parts.length === 0 ? null : parts.join(' · ');
}

/** A row of text buttons under a bubble (Retry, Dismiss, Send again). */
function RowActions({ actions }: { actions: { label: string; onPress: () => void }[] }) {
  if (actions.length === 0) return null;
  return (
    <View style={styles.actions}>
      {actions.map((a) => (
        <Button key={a.label} title={a.label} variant="tonal" size="small" onPress={a.onPress} />
      ))}
    </View>
  );
}

export function PersistedMessage({
  message,
  tools,
  onOpenTool,
  highlight,
  opening,
  onOpenFile,
  onActions,
  avatar,
  actions,
}: {
  message: ChatMessage;
  onActions: () => void;
  highlight: { people: string[]; agents: string[] };
  opening: string | null;
  onOpenFile: ((attachment: ChatAttachment) => void) | null;
  tools: readonly ToolStep[];
  onOpenTool: (step: ToolStep) => void;
  /** The answering agent's avatar (answers only). */
  avatar?: ReactNode;
  /** The answer's action row (finished answers only). */
  actions?: ReactNode;
}) {
  const status = message.isStreaming
    ? 'Still generating…'
    : message.hasUnsupportedItems
      ? 'Part of this message can only be viewed in the web app.'
      : null;
  const answer = message.role === 'assistant';
  return (
    <MessageBubble
      role={message.role}
      mine={message.mine}
      author={message.author}
      meta={answer || !message.mine ? messageMeta(answer ? answerAuthorLine(message.author, message.authorRole, message.messageClass) : message.author, message.createdAt) : null}
      avatar={answer ? avatar : undefined}
      status={status}
      error={message.error}
      highlight={message.role === 'user' ? highlight : null}
      onActions={message.text.trim() !== '' || answer ? onActions : null}
      actions={answer && !message.isStreaming ? actions : undefined}
      footer={
        message.attachments.length > 0 || tools.length > 0 ? (
          <>
            <AttachmentStrip attachments={message.attachments} mine={message.mine} opening={opening} onOpen={onOpenFile} />
            {tools.length > 0 ? <ToolActivity steps={tools} onOpen={onOpenTool} /> : null}
          </>
        ) : null
      }>
      {answer && message.text !== '' ? <Markdown value={message.text} /> : message.text}
    </MessageBubble>
  );
}

/** What a refused outbox item carried besides its text (kept on "Send again"). */
function extrasOf(item: OutboxItem): SendExtras {
  try {
    const { mentions, attachments, addressedTo } = restorePreparedTurn(item.bodyJson).request;
    // Refused for an addressee that left: sent again, the floor decides instead.
    return { mentions, attachments, addressedTo: item.lastError === 'addressee_not_in_conversation' ? null : addressedTo };
  } catch (error) {
    if (error instanceof TurnInputError) return {};
    throw error;
  }
}

/**
 * A platform line (ADR-0050 decision 7e: a loop guard, a hand-off): centred, muted, no avatar and
 * no bubble.
 */
export function SystemLine({ message }: { message: ChatMessage }) {
  return (
    <View style={styles.system} accessible accessibilityRole="text" testID="system-line">
      <Text variant="footnote" tone="secondary" style={styles.systemText}>
        {message.text}
      </Text>
    </View>
  );
}

/**
 * A run of agent-to-agent messages (decision 7e) folded into one "Side conversation (n)" row; a
 * tap opens it into compact "A → B" rows, each with its text.
 */
export function SideTalkRow({ messages, nameOf }: { messages: readonly ChatMessage[]; nameOf: (participantId: number) => string | null }) {
  const { colors, radius } = useDesign();
  const [open, setOpen] = useState(false);
  return (
    <View style={styles.side}>
      <Pressable
        accessibilityRole="button"
        accessibilityState={{ expanded: open }}
        accessibilityHint={TURN_TAKING_COPY.sideConversationHint(open)}
        onPress={() => setOpen((v) => !v)}
        style={[styles.sideHeader, { backgroundColor: colors.surfaceSunken, borderRadius: radius.md }]}>
        <Icon name={open ? 'chevronDown' : 'chevronRight'} size={12} tone="secondary" weight="semibold" />
        <Text variant="footnote" tone="secondary" emphasized>
          {TURN_TAKING_COPY.sideConversation(messages.length)}
        </Text>
      </Pressable>
      {open
        ? messages.map((m) => (
            <View key={m.key} style={[styles.sideRow, { borderStartColor: colors.separatorStrong }]} testID="side-talk-row">
              <Text variant="caption" tone="secondary" emphasized numberOfLines={1}>
                {sideTalkLabel(m.author ?? '', (m.addressedTo ?? []).map((id) => nameOf(id) ?? ''))}
              </Text>
              <Text variant="footnote" numberOfLines={6}>
                {m.text}
              </Text>
            </View>
          ))
        : null}
    </View>
  );
}

/** A message waiting in the offline outbox (queued, sending, or refused). */
export function QueuedMessage({
  item,
  onSendAgain,
}: {
  item: OutboxItem;
  onSendAgain: (text: string, extras?: SendExtras) => Promise<string | null>;
}) {
  const status = outboxStatus(item);
  const actions =
    item.state === 'failed'
      ? [
          // A new message with a new question_id; the refused one is dropped
          // only once the new one is accepted, so a failed resend loses nothing.
          {
            label: 'Send again',
            onPress: () =>
              void onSendAgain(item.userText, extrasOf(item)).then((problem) => {
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
      actions={<RowActions actions={actions} />}>
      {item.userText}
    </MessageBubble>
  );
}

/** The answer being written now: the thinking state, the streaming text with its caret, or why it stopped. */
export function LiveReply({
  live,
  author,
  avatar,
  onRetry,
  onReconnect,
  onDismiss,
  onOpenTool,
  usageAvailable,
}: {
  live: LiveTurn;
  author: string | null;
  avatar?: ReactNode;
  /** Null: a failed turn is not retried from here (the thread cannot take a message now). */
  onRetry: (() => void) | null;
  onReconnect: () => void;
  onDismiss: () => void;
  onOpenTool: (step: ToolStep) => void;
  /** Settings has a usage card (client contract 1.3). Older servers refuse on budget too, but have no usage to show. */
  usageAvailable: boolean;
}) {
  const { turn } = live;
  const waiting = turn.phase === 'sending' || (turn.phase === 'streaming' && turn.content === '');
  const status =
    turn.status ??
    (turn.stopped
      ? 'Stopped.'
      : turn.phase === 'paused'
        ? turn.pause
          ? 'Waiting for you.'
          : 'Paused. Continue it in the web app.'
        : turn.phase === 'sending'
          ? 'Sending…'
          : waiting
            ? 'Thinking…'
            : live.streamStatus === 'retrying'
              ? 'Reconnecting…'
              : null);
  const actions = [
    live.canReconnect ? { label: 'Reconnect', onPress: onReconnect } : null,
    turn.phase === 'failed' && turn.error?.retryable && onRetry ? { label: 'Retry', onPress: onRetry } : null,
    turn.phase === 'failed' && turn.error?.budget && usageAvailable ? { label: 'View usage', onPress: () => router.dismissTo('/settings') } : null,
    turn.phase === 'failed' ? { label: 'Dismiss', onPress: onDismiss } : null,
  ].filter((a): a is { label: string; onPress: () => void } => a !== null);
  const streaming = turn.phase === 'streaming' && turn.content !== '';
  return (
    <MessageBubble
      role="assistant"
      author={author}
      meta={author}
      avatar={avatar}
      status={status}
      error={turn.phase === 'failed' ? (turn.error?.message ?? null) : null}
      actions={<RowActions actions={actions} />}
      footer={<ToolActivity steps={turn.tools} streaming={turn.phase === 'streaming'} onOpen={onOpenTool} />}>
      {turn.content !== '' ? <Markdown value={turn.content} streaming={turn.phase === 'streaming'} caret={streaming} /> : waiting && !turn.stopped ? <ThinkingDots /> : ''}
    </MessageBubble>
  );
}

const styles = StyleSheet.create({
  actions: { flexDirection: 'row', flexWrap: 'wrap', gap: 8 },
  system: { alignSelf: 'center', maxWidth: '85%', paddingVertical: 6, paddingHorizontal: 12 },
  systemText: { textAlign: 'center' },
  side: { alignSelf: 'stretch', gap: 6, paddingStart: 40, paddingVertical: 4 },
  sideHeader: { alignSelf: 'flex-start', flexDirection: 'row', alignItems: 'center', gap: 6, minHeight: 32, paddingHorizontal: 10 },
  sideRow: { gap: 2, paddingStart: 10, borderStartWidth: 2 },
});
