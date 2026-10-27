/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-box/
 * ChatMessageList.jsx` (302 lines) — renders the full message list for a
 * conversation, orchestrating individual message row rendering.
 *
 * Folds the baseline's `ChatMessageWrapper.jsx` (a per-message controller:
 * derive `isLastMessage`-gated callbacks, route to `UserMessage` vs
 * `ApplicationAnswer`) directly into this file's render loop rather than a
 * separate wrapper component — see `ChatMessageWrapper.tsx`'s deletion note
 * and this feature's `index.ts` barrel.
 *
 * Related props are grouped into option objects (`messageActions`/`tts`/
 * `continuation`/`pagination`) to stay under the §3.5 component-props
 * budget, mirroring the same grouping pattern used by sibling
 * `ApplicationAnswer.tsx`.
 */
import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef } from 'react';

import Box from '@mui/material/Box';
import Skeleton from '@mui/material/Skeleton';

import { ApplicationAnswer } from './ApplicationAnswer';
import { SteeringMessages } from './SteeringMessages';
import { UserMessage } from './UserMessage';
import { useStickToBottom } from './useStickToBottom';

import { useChatSessionStore } from '@/entities/conversation';
import { WELCOME_MESSAGE_ID } from '@/shared/lib/enums';
import { ScrollableContainer } from '@/shared/ui/ScrollableContainer';
import type { SimpleBarInstance } from '@/shared/ui/ScrollableContainer';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';
import { TraceStepDetailProvider } from '../../model/traceStepDetail';
import { t } from '@/shared/i18n';
import { CONVERSATION_COLUMN } from '@/shared/ui/conversation-column';

import type { ChatMessageListProps } from './ChatMessageList.types';

// `ChatMessageListProps` and the per-group prop interfaces it composes
// (`ChatMessageListActions`/`Canvas`/`Tts`/`Continuation`/`Pagination`) live
// in `./ChatMessageList.types` — split out purely to keep this file under
// the §3.5 file-length budget, same rationale as `ApplicationAnswer.types.ts`.
// `features/chat-messages/index.ts` imports `ChatMessageListCanvas`/
// `ChatMessageListProps` directly from `./ChatMessageList.types`, not
// through this re-export.
export type {
  ChatMessageListActions,
  ChatMessageListCanvas,
  ChatMessageListContinuation,
  ChatMessageListPagination,
  ChatMessageListProps,
  ChatMessageListTts,
} from './ChatMessageList.types';

/** How close to the top (px) a scroll position must be to fire `onScrollToTop` — baseline: `ChatMessageList.jsx`'s own `scrollTop <= 20`. */
const SCROLL_TOP_THRESHOLD = 20;

/**
 * The author id a value actually STATES, or `undefined` when it states none.
 *
 * `''` is not an identity: `entities/message/lib/normalise.ts` uses the empty
 * string as "the attribution STOPS here" for a row with no author, and
 * `ChatBox.tsx` passes `userId={userId ?? ''}` while `useGetCurrentAuthor` is
 * still in flight (or forever, if it fails). Both spellings of "unstated"
 * therefore have to collapse to one before any comparison.
 *
 * `String()`-normalised for the same reason `normaliseUserMessage` documents:
 * `message.userId` is always a string, but the reader's id comes from the
 * unvalidated `GET /social/author` envelope, whose `id` the transcript and
 * participants payloads state as a NUMBER.
 */
function statedAuthorId(value: string | undefined): string | undefined {
  if (value === undefined || value === '') return undefined;
  return String(value);
}

/**
 * Whether the reader may act on a message authored by `authorId`.
 *
 * The two unstated sides are deliberately NOT symmetric:
 *
 * - reader unstated ⇒ PERMIT. This is the documented pre-identity escape
 *   hatch — no reader has been stated, so the surface keeps the behaviour it
 *   had before author identity existed rather than refusing everything. It was
 *   unreachable until `statedAuthorId` above, because `ChatBox` renders `''`
 *   rather than `undefined`, so a reader whose author query was merely PENDING
 *   was refused edit and delete on their own messages with no explanation.
 * - author unstated ⇒ REFUSE. An unattributed row must not become editable
 *   and deletable by everyone in a shared conversation; absent attribution is
 *   the one case where refusing is the safe answer.
 */
function isOwnMessage(readerId: string | undefined, authorId: string | undefined): boolean {
  const reader = statedAuthorId(readerId);
  if (reader === undefined) return true;
  const author = statedAuthorId(authorId);
  if (author === undefined) return false;
  return reader === author;
}

/**
 * Which side a question sits on: someone else's (left) only when BOTH the
 * reader and the author are stated and differ. Layout is not a permission, so
 * an unstated side does not refuse the way `isOwnMessage` must — a reloaded
 * row the server attributed to nobody is far more likely the reader's own
 * question in their own chat than a stranger's, and drawing it on the left
 * would make every such conversation look like someone else's.
 */
function isOtherAuthor(readerId: string | undefined, authorId: string | undefined): boolean {
  const reader = statedAuthorId(readerId);
  const author = statedAuthorId(authorId);
  return reader !== undefined && author !== undefined && reader !== author;
}

/** Baseline `ChatMessageWrapper.jsx`'s `canDeleteThisAIMessage` — whether `userId` authored the question this AI answer replies to. */
function canDeleteAiMessage(
  chatHistory: readonly ChatMessage[],
  message: ChatMessage,
  userId: string | undefined,
): boolean {
  const question = chatHistory.find((item) => item.id === message.questionId);
  return isOwnMessage(userId, question?.userId);
}

/**
 * `ChatMessageList` — renders the full message list for a conversation.
 * Each message is rendered according to its role (user / assistant), with
 * per-message callback gating (delete/regenerate/continue/HITL) folded in
 * from the baseline's `ChatMessageWrapper`.
 */
// eslint-disable-next-line eslint/complexity, eslint/max-lines-per-function -- per-message gating folded in from ChatMessageWrapper; oxlint's complexity+max-lines are already disabled repo-wide for this directory (.oxlintrc.json)
export function ChatMessageList({
  chatHistory,
  isStreaming = false,
  userId,
  projectId,
  messageActions: { onCopyToClipboard, onDeleteAnswer, onRegenerateAnswer, onSubmitEditedMessage, onForkFromMessage } = {},
  canvas: { onEdit: onEditCanvas, selected: selectedCodeBlockInfo, onCreateFromSelection: onCreateCanvasFromSelection, onOpenFile: onOpenFileInCanvas } = {},
  tts: { autoSpeak = false, onAutoSpeak, speakingMessageId, speakingSegments, spokenRange } = {},
  continuation: {
    onContinueMcpExecution,
    renderAuthModal,
    onContinueTokenLimitExecution,
    onHitlResume,
    hideContinueButton = false,
    hideHitlActions = false,
    staticPause,
  } = {},
  pagination: { isLoadingMore = false, onScrollToTop } = {},
  emptyState,
  assistantName,
  steering,
}: ChatMessageListProps): ReactNode {
  const scrollRef = useRef<SimpleBarInstance | null>(null);
  const listRef = useRef<HTMLUListElement | null>(null);
  const messageRowRefs = useRef<Record<string, HTMLElement | null>>({});

  const messageIdToView = useChatSessionStore((state) => state.messageIdToView);
  const setMessageIdToView = useChatSessionStore((state) => state.setMessageIdToView);

  const setMessageRowRef = useCallback(
    (id: string) => (el: HTMLElement | null) => {
      messageRowRefs.current[id] = el;
    },
    [],
  );

  // Baseline `getOnSubmit` gating (ChatMessageList.jsx): edit-and-resubmit is
  // only offered for the single last user-role message, owned by the current
  // user, while nothing is streaming. `isLoading` isn't in this component's
  // props yet (ChatBox.tsx doesn't supply it) so it's omitted here rather
  // than invented.
  // Steering (contract 1.27): an injected message that the transcript already holds is not shown two times.
  const transcriptIds = useMemo(() => new Set(chatHistory.map((message) => message.id)), [chatHistory]);
  // A name over each reply only when more than one assistant answers here.
  const isMultiAssistant = useMemo(() => {
    const names = new Set(chatHistory.filter((msg) => msg.role !== 'user').map((msg) => msg.authorName ?? assistantName ?? ''));
    names.delete('');
    return names.size > 1;
  }, [chatHistory, assistantName]);
  const lastUserMessageIndex = useMemo(
    () => chatHistory.reduce((last, msg, i) => (msg.role === 'user' ? i : last), -1),
    [chatHistory],
  );

  // Land at the bottom on open/send and follow a streaming reply while the
  // reader stays at the bottom — see `useStickToBottom` for why the message
  // count alone (the baseline's `[activeConversation?.id]` effect) is not enough.
  useStickToBottom({
    scrollRef,
    contentRef: listRef,
    firstId: chatHistory[0]?.id,
    lastId: chatHistory.at(-1)?.id,
    count: chatHistory.length,
  });

  useEffect(() => {
    const scrollEl = scrollRef.current?.getScrollElement();
    if (!scrollEl || !onScrollToTop) return undefined;
    const handleScroll = (): void => {
      if (scrollEl.scrollTop <= SCROLL_TOP_THRESHOLD) onScrollToTop();
    };
    scrollEl.addEventListener('scroll', handleScroll);
    return () => scrollEl.removeEventListener('scroll', handleScroll);
  }, [onScrollToTop]);

  useEffect(() => {
    if (!messageIdToView || !chatHistory.length) return;
    const target = messageRowRefs.current[messageIdToView];
    if (target) {
      target.scrollIntoView({ behavior: 'smooth', block: 'center' });
      setMessageIdToView('');
    }
  }, [messageIdToView, chatHistory, setMessageIdToView]);

  if (!chatHistory?.length) {
    return (
      <Box
        sx={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          py: 4,
          color: 'text.secondary',
        }}
      >
        {emptyState ?? t('features.chatMessages.noMessages', 'No messages yet')}
      </Box>
    );
  }

  return (
    <TraceStepDetailProvider projectId={projectId}>
    <ScrollableContainer ref={scrollRef}>
      {/*
        * A plain `<ul>`, NOT MUI's `<List>`: `shared/brand/mui-overrides/
        * MuiList.ts` styles the untagged `MuiList` variant as a MENU card
        * (`background.secondary` + a `border.lines` hairline + the default
        * shadow + `min-width: 12.5rem`), and that key is what `<List>`
        * reads — so the whole transcript rendered inside a floating
        * menu-shaped panel, which the production UI does not have. The
        * override's own doc claims "`<List>` is not used unwrapped anywhere
        * else in this app (grepped)"; this file was that use.
        *
        * Padding matches the baseline's `MessageList`
        * (`components/Chat/StyledComponents.jsx:108-114`:
        * `padding: 0 0.75rem 0.75rem`).
        */}
      <Box
        component="ul"
        ref={listRef}
        data-testid="chat-message-list"
        sx={{ ...CONVERSATION_COLUMN, flexGrow: 1, listStyle: 'none', marginY: 0, paddingTop: 2, paddingBottom: 1.5 }}
      >
        {isLoadingMore && (
          <Box component="li" sx={{ display: 'flex', flexDirection: 'column', gap: 1, pb: 1, listStyle: 'none' }}>
            {Array.from({ length: 3 }).map((_, index) => (
              // eslint-disable-next-line react/no-array-index-key -- fixed-length placeholder rows, no stable identity
              <Skeleton key={index} animation="wave" variant="rounded" sx={{ width: '100%', height: '3rem' }} />
            ))}
          </Box>
        )}
        {chatHistory.map((message, index) => {
          const isUser = message.role === 'user';
          const messageId = message.id;
          const isLastMessage = index === chatHistory.length - 1;
          /*
           * `isLastMessage && isStreaming` is a FALLBACK for the few frames
           * between a turn's optimistic append and the reducer's first
           * `isStreaming` stamp on THIS message — a brand-new placeholder
           * with no answer in it yet. It must not outlive that window: the
           * page-level `isStreaming` this ORs in is itself an OR of three
           * sources (composer Stop, persisted-history catch-up, the live
           * transport — see `ChatBox.tsx`'s own comment on it), and none of
           * them is guaranteed to settle back to `false` the instant THIS
           * message's own turn ends — the persisted-history source in
           * particular is read off a conversation-messages fetch that a
           * finished turn does not itself invalidate. A last message that
           * already carries an answer (text, or a stored canvas item) is
           * proof the placeholder window is over, so its OWN state — not a
           * page-level flag with three unrelated reasons to be `true` — must
           * decide whether the reader can still act on it. Before this
           * guard, a stuck/late page-level flag permanently hid the "Create
           * canvas" selection control (and disabled regenerate) on the very
           * last message of a conversation that had visibly finished
           * answering.
           */
          const hasRenderedAnswer = Boolean(message.content) || (message.messageItems?.length ?? 0) > 0;
          const messageIsStreaming = Boolean(message.isStreaming) || (isLastMessage && isStreaming && !hasRenderedAnswer);
          // Ownership goes through `isOwnMessage` on every branch, so the
          // String-normalisation and the unstated-reader/unstated-author rules
          // are decided in exactly one place (see its docblock).
          const isEligibleForEdit =
            isUser && index === lastUserMessageIndex && !isStreaming && isOwnMessage(userId, message.userId);
          const canDeleteMessage = isUser
            ? isOwnMessage(userId, message.userId)
            : canDeleteAiMessage(chatHistory, message, userId);
          const handleDelete =
            isLastMessage && message.id !== WELCOME_MESSAGE_ID && !message.isSummarized && canDeleteMessage && onDeleteAnswer
              ? () => { onDeleteAnswer(messageId); }
              : undefined;
          const handleCopy = onCopyToClipboard ? () => { onCopyToClipboard(message); } : undefined;
          // A fork point must be settled on the server: not streaming, not loading, not waiting for an approval, and stored (not the welcome row).
          const isSettled = !messageIsStreaming && !message.isLoading && !message.isRegenerating && message.hitlInterrupt === undefined && (message.hitlInterrupts?.length ?? 0) === 0 && message.staticPause === undefined;
          const handleFork = onForkFromMessage && isSettled && message.id !== WELCOME_MESSAGE_ID ? () => { onForkFromMessage(messageId); } : undefined;

          return (
            <Box
              component="li"
              key={messageId}
              data-testid="chat-message-item"
              ref={setMessageRowRef(messageId)}
              // The one gap between turns: the rows carry no padding of their own.
              sx={{ listStyle: 'none', width: '100%', mb: 2 }}
            >
              {isUser ? (
                <UserMessage
                  message={message}
                  messageId={messageId}
                  isOwn={!isOtherAuthor(userId, message.userId)}
                  projectId={projectId}
                  onOpenFileInCanvas={onOpenFileInCanvas}
                  onCopy={handleCopy}
                  onDelete={handleDelete}
                  onFork={handleFork}
                  onSubmit={isEligibleForEdit ? onSubmitEditedMessage : undefined}
                />
              ) : (
                <ApplicationAnswer
                  answer={message}
                  messageId={messageId}
                  isLastMessage={isLastMessage}
                  // issue 974: the flag that lets this row's auto-read effect fire.
                  // It was declared on the `tts` prop group and destructured
                  // nowhere, so speaking mode reached the composer and never
                  // the answers — the mode's whole point (reading the reply
                  // back) could not happen.
                  isSpeakingMode={autoSpeak}
                  author={{ participantName: message.authorName ?? assistantName, showCaption: isMultiAssistant }}
                  toolActions={message.toolActions}
                  status={{ isLoading: Boolean(message.isLoading), isStreaming: messageIsStreaming }}
                  actions={{
                    onCopy: handleCopy,
                    onDelete: handleDelete,
                    onFork: handleFork,
                    onRegenerate:
                      isLastMessage && !message.isLoading && !messageIsStreaming && onRegenerateAnswer
                        ? () => { onRegenerateAnswer(messageId); }
                        : undefined,
                    shouldDisableRegenerate: messageIsStreaming || Boolean(message.isLoading) || message.id === WELCOME_MESSAGE_ID,
                    onEditCanvas, selectedCodeBlockInfo, onCreateCanvasFromSelection,
                  }}
                  continuation={{
                    hideContinueButton,
                    onContinueMcpExecution: isLastMessage ? onContinueMcpExecution : undefined,
                    renderAuthModal,
                    onContinueTokenLimitExecution: isLastMessage ? onContinueTokenLimitExecution : undefined,
                  }}
                  hitl={{
                    hitlInterrupt: hideHitlActions ? undefined : message.hitlInterrupt,
                    hitlInterrupts: hideHitlActions ? undefined : message.hitlInterrupts,
                    onHitlResume: !hideHitlActions && isLastMessage ? onHitlResume : undefined,
                    staticPause: staticPause?.pause.messageId === messageId ? staticPause : undefined,
                  }}
                  tts={{
                    onAutoSpeak,
                    ...(speakingMessageId && { speakingMessageId }),
                    ...(speakingSegments && { speakingSegments }),
                    ...(spokenRange && { spokenRange }),
                  }}
                  feedback={{
                    projectId,
                    enabled: message.id !== WELCOME_MESSAGE_ID && !message.isSummarized,
                  }}
                />
              )}
              {!isUser && steering && (
                <SteeringMessages responseMessageId={messageId} transcriptIds={transcriptIds} isLive={messageIsStreaming || Boolean(message.isLoading)} model={steering.model} onStopAndSend={steering.onStopAndSend} />
              )}
            </Box>
          );
        })}
      </Box>
    </ScrollableContainer>
    </TraceStepDetailProvider>
  );
}
