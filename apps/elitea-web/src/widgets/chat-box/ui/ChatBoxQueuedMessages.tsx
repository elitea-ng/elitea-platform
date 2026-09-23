/**
 * A17 — the "Waiting messages" strip above the composer, and the hook that
 * drives it (ELITEA-2864…2874).
 *
 * The hook owns three things the composition root would otherwise have to:
 *
 *  1. WHERE A SEND GOES. While a run is open the composer stays live and its
 *     text is QUEUED; otherwise it goes straight down the ordinary send path.
 *     `onSend` is what `ChatBox` hands to `NewChatInput`, so there is exactly
 *     one decision point and no way for a caller to bypass it.
 *  2. WHEN THE QUEUE DRAINS. One message is delivered per SETTLE — the same
 *     "the composer was released" signal every other part of this surface uses
 *     (`isStreaming` falling) — and the next one waits for the turn that
 *     delivery starts. That is what keeps the order strictly FIFO
 *     (ELITEA-2865) and what makes a Stop deliver the queue rather than drop
 *     it (ELITEA-2868): Stop settles the run like any other ending.
 *
 *     Delivery goes through `sendQuestion`, NOT through a second transport:
 *     admission, budget, guardrails and participant resolution all still apply
 *     to a queued message exactly as they do to a typed one. The server is the
 *     one that decides whether the turn is allowed; this only decides when to
 *     ask.
 *  3. WHICH QUESTIONS WERE INTERJECTIONS. `decorate` stamps `interjected` onto
 *     the transcript rows whose ids the store recorded, so the "Sent while
 *     running" caption survives the round trip through the server — the id is
 *     the client-generated `question_id`, which the start route persists as
 *     the question row's own uuid.
 *
 * DELIVERY CANNOT WEDGE. Each pass removes exactly one row from the queue
 * before it awaits anything, so a delivery that fails outright (no transport,
 * a refusal) still shortens the queue and the next pass moves on. The `sending`
 * flag exists only to stop two passes overlapping, and it is STATE rather than
 * a ref precisely so clearing it re-runs the effect — a ref would leave the
 * remainder of the queue stranded whenever a send failed without ever opening
 * a stream.
 */
import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';
import CloseIcon from '@mui/icons-material/Close';

import type { ChatMessage } from '@/features/chat-messages';
import { t } from '@/shared/i18n';

import {
  DRAFT_CONVERSATION_KEY,
  useQueuedMessagesStore,
  type QueuedChatMessage,
} from '../model/queuedMessages.store';

/** What one delivery reports back — `useChatBoxHandlers`' own `SendResult`, narrowed to the fields this model reads. */
export interface QueuedSendOutcome {
  readonly questionId?: string | undefined;
  readonly createdConversation?: { readonly id?: string | number; readonly uuid?: string } | undefined;
}

/** The queue's key for a conversation: its uuid, else its numeric id, else the draft sentinel (a chat with no row yet). */
export function chatBoxQueueKey(conversationUuid: string | undefined, conversationId: string | number | undefined): string {
  return conversationUuid ?? (conversationId === undefined ? DRAFT_CONVERSATION_KEY : String(conversationId));
}

export interface UseChatBoxQueuedMessagesParams {
  /** The conversation the queue belongs to — see `chatBoxQueueKey`. */
  readonly conversationKey: string;
  /** A turn is open. The composer does not block on it; the queue does. */
  readonly isStreaming: boolean;
  /** The ordinary composer send, used when nothing is running. */
  readonly onSendNow: (question: string) => void;
  /** The ORDINARY send path (`useChatBoxHandlers`' `sendQuestion`) a queued message is delivered down — never a second transport. */
  readonly send: (params: { readonly question: string }) => Promise<QueuedSendOutcome>;
  /** Announced when a delivery is what finally committed the conversation row — the same contract the typed send has. */
  readonly onConversationCreated?: ((conversation: { readonly id?: string | number; readonly uuid?: string }) => void) | undefined;
}

export interface ChatBoxQueuedMessagesModel {
  readonly items: readonly QueuedChatMessage[];
  /** What `NewChatInput` calls: queue while a run is open, send otherwise. */
  readonly onSend: (question: string) => void;
  readonly onRemove: (id: string) => void;
  /** Stamps `interjected` on the transcript rows delivered from this queue. */
  readonly decorate: (messages: readonly ChatMessage[]) => readonly ChatMessage[];
}

const EMPTY_QUEUE: readonly QueuedChatMessage[] = [];
const EMPTY_IDS: readonly string[] = [];

export function useChatBoxQueuedMessages(params: UseChatBoxQueuedMessagesParams): ChatBoxQueuedMessagesModel {
  const { conversationKey, isStreaming, onSendNow, send, onConversationCreated } = params;
  const items = useQueuedMessagesStore((state) => state.queues[conversationKey] ?? EMPTY_QUEUE);
  const interjectedIds = useQueuedMessagesStore((state) => state.interjected[conversationKey] ?? EMPTY_IDS);
  const [sending, setSending] = useState(false);

  // Read through refs so the delivery effect below depends on the QUEUE and on
  // the run, not on callback identities the composition root rebuilds every
  // render (which would restart the effect mid-delivery).
  const streamingRef = useRef(isStreaming);
  streamingRef.current = isStreaming;
  const sendNowRef = useRef(onSendNow);
  sendNowRef.current = onSendNow;
  const sendRef = useRef(send);
  sendRef.current = send;
  const createdRef = useRef(onConversationCreated);
  createdRef.current = onConversationCreated;

  // The draft chat's queue follows the conversation the first send created.
  const previousKeyRef = useRef(conversationKey);
  useEffect(() => {
    const previous = previousKeyRef.current;
    previousKeyRef.current = conversationKey;
    if (previous === conversationKey || previous !== DRAFT_CONVERSATION_KEY) return;
    useQueuedMessagesStore.getState().adopt(previous, conversationKey);
  }, [conversationKey]);

  const onSend = useCallback(
    (question: string) => {
      if (question.trim() === '') return;
      if (!streamingRef.current) {
        sendNowRef.current(question);
        return;
      }
      useQueuedMessagesStore.getState().enqueue(conversationKey, question);
    },
    [conversationKey],
  );

  const onRemove = useCallback(
    (id: string) => {
      useQueuedMessagesStore.getState().remove(conversationKey, id);
    },
    [conversationKey],
  );

  const queueLength = items.length;
  useEffect(() => {
    if (isStreaming || sending || queueLength === 0) return;
    const store = useQueuedMessagesStore.getState();
    const next = store.dequeue(conversationKey);
    if (next === undefined) return;
    setSending(true);
    void sendRef
      .current({ question: next.text })
      .then((outcome) => {
        if (outcome.questionId) {
          useQueuedMessagesStore.getState().markInterjected(conversationKey, outcome.questionId);
        }
        // The row is committed before any transport is tried, so a delivery
        // that then fails still leaves a conversation the route and the rail
        // have to learn about — the same rule the typed send follows.
        if (outcome.createdConversation) createdRef.current?.(outcome.createdConversation);
      })
      .catch(() => undefined)
      .finally(() => {
        setSending(false);
      });
  }, [conversationKey, isStreaming, queueLength, sending]);

  const decorate = useMemo(() => {
    const marked = new Set(interjectedIds);
    return (messages: readonly ChatMessage[]): readonly ChatMessage[] => {
      if (marked.size === 0) return messages;
      let changed = false;
      const next = messages.map((message) => {
        if (!marked.has(message.id) || message.interjected === true) return message;
        changed = true;
        return { ...message, interjected: true };
      });
      return changed ? next : messages;
    };
  }, [interjectedIds]);

  return { items, onSend, onRemove, decorate };
}

/* ── the strip ───────────────────────────────────────────────────────────── */

function stripSx(theme: Theme) {
  return {
    display: 'flex',
    flexDirection: 'column',
    gap: '0.25rem',
    padding: '0.5rem 0.75rem',
    marginBottom: '0.5rem',
    borderRadius: theme.vars.shape.radiusMd,
    backgroundColor: theme.vars.palette.background.aiAnswerBkg,
  } as const;
}

const rowSx = {
  display: 'flex',
  alignItems: 'center',
  gap: '0.5rem',
  width: '100%',
} as const;

const textSx = {
  flex: 1,
  minWidth: 0,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
} as const;

export interface ChatBoxQueuedMessagesProps {
  readonly queue: ChatBoxQueuedMessagesModel;
}

/**
 * Renders nothing while the queue is empty (ELITEA-2869's "queue area hides
 * when empty" — the strip is absent, not merely blank, so a reader cannot
 * mistake an empty box for a lost message).
 */
export function ChatBoxQueuedMessages({ queue }: ChatBoxQueuedMessagesProps): ReactNode {
  const { items, onRemove } = queue;
  if (items.length === 0) return null;
  const removeLabel = t('widgets.chatBox.queuedRemove', 'Remove from the waiting list');
  return (
    <Box
      sx={stripSx}
      data-testid="chat-queued-messages"
    >
      <Typography
        variant="labelMedium"
        color="text.secondary"
        data-testid="chat-queued-count"
      >
        {t('widgets.chatBox.queuedHeading', 'Waiting messages · {{count}}', { count: items.length })}
      </Typography>
      {items.map((item) => (
        <Box
          key={item.id}
          sx={rowSx}
          data-testid="chat-queued-item"
        >
          <Typography
            variant="bodyMedium"
            color="text.primary"
            sx={textSx}
            data-testid="chat-queued-text"
          >
            {item.text}
          </Typography>
          <Typography
            variant="labelSmall"
            color="text.secondary"
            data-testid="chat-queued-status"
          >
            {t('widgets.chatBox.queuedStatus', 'Queued')}
          </Typography>
          <Tooltip
            title={removeLabel}
            placement="top"
          >
            <IconButton
              size="small"
              aria-label={removeLabel}
              data-testid="chat-queued-remove"
              onClick={() => { onRemove(item.id); }}
            >
              <CloseIcon fontSize="small" />
            </IconButton>
          </Tooltip>
        </Box>
      ))}
    </Box>
  );
}
