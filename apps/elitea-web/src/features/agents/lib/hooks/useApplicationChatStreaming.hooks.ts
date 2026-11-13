import { useCallback, useMemo, useRef } from 'react';

import { t } from '@/shared/i18n';
import { NAME_MAX_LENGTH, clipToLimit } from '@/shared/lib/textLimits';
import { ROLES } from '@/shared/lib/enums';
import type { SocketClient } from '@/shared/api/socket/client';
import type { Participant } from '@/entities/participant';

import { applicationErrorMessageOrFallback } from '../errorMessage';
import { getInitialChatHistory, isMessageInFlight } from './applicationChat.helpers';
import type { ChatApplicationVersionDetails, ChatConversation, ChatConversationAdapter, ChatHistoryMessage } from './applicationChat.types';

/**
 * Streaming/delete slice of `useApplicationChat` — `isStreaming` tracking,
 * `onStopStreaming`/`onStopAll`, `onDeleteMessage`/`onDeleteAllMessages`.
 * Split out of `useApplicationChat.hooks.ts` purely to keep every function
 * under this codebase's `complexity`/`max-lines` gates (see that file's own
 * module doc comment for the full baseline citation and disclosed-
 * deviation list — this split changes no behaviour).
 */
export interface UseApplicationChatStreamingParams {
  readonly socket: SocketClient;
  readonly adapter: ChatConversationAdapter;
  readonly projectId: string | undefined;
  readonly applicationName: string | undefined;
  readonly applicationVersionDetails: ChatApplicationVersionDetails | undefined;
  readonly applicationParticipant: Participant | null;
  readonly activeConversation: ChatConversation | null;
  readonly activeParticipantId: string | number | undefined;
  readonly chatHistoryRef: React.RefObject<ChatHistoryMessage[]>;
  readonly setChatHistory: (
    update: ChatHistoryMessage[] | ((prev: ChatHistoryMessage[]) => ChatHistoryMessage[]),
  ) => void;
  readonly setActiveConversation: (c: ChatConversation) => void;
  readonly onInfo?: ((message: string) => void) | undefined;
  readonly onError?: ((message: string) => void) | undefined;
}

export interface UseApplicationChatStreamingResult {
  readonly isStreaming: boolean;
  readonly onStopStreaming: (message: ChatHistoryMessage) => () => Promise<void>;
  readonly onStopAll: () => Promise<void>;
  readonly onDeleteMessage: (messageIdToDelete: string | number, callback?: () => void) => Promise<void>;
  readonly onDeleteAllMessages: (callback?: () => void) => Promise<void>;
}

/**
 * Derived directly off `activeConversation.chat_history` — a plain
 * `useMemo`, no separate tracked state. **Disclosed simplification:** the
 * baseline (`useApplicationChat.hooks.js`) ALSO writes a redundant
 * `streamingState.streamingMessages` `Set` as a side effect of this exact
 * computation, but never exposes it (only `isStreaming: boolean` is
 * returned) and never reads it anywhere except inside its own
 * `onStopStreaming`, itself just deriving a boolean from the Set's size —
 * a value already recomputed fresh from `chat_history` on the very next
 * render once `setChatHistory` flips the message's `isStreaming` field.
 * Dropped as genuinely redundant bookkeeping, not a behaviour change to
 * anything this hook (or the baseline) actually exposes.
 */
function useIsStreaming(activeConversation: ChatConversation | null): boolean {
  return useMemo(() => activeConversation?.chat_history.some(isMessageInFlight) ?? false, [activeConversation?.chat_history]);
}

function clearStreamFields(msg: ChatHistoryMessage, streamId: string | number): ChatHistoryMessage {
  return {
    ...msg,
    isStreaming: msg.id === streamId ? false : msg.isStreaming,
    isLoading: msg.id === streamId ? false : msg.isLoading,
    task_id: msg.id === streamId ? undefined : msg.task_id,
  };
}

/** The fresh-conversation literal `onDeleteAllMessages` replaces `activeConversation` with — extracted purely to shrink that callback's own body (its dependency-array size is fixed separately via `paramsRef`, see that callback's own comment). */
function buildFreshConversation(
  applicationName: string | undefined,
  applicationParticipant: Participant | null,
  applicationVersionDetails: ChatApplicationVersionDetails | undefined,
  activeParticipantId: string | number | undefined,
): ChatConversation {
  return {
    name: clipToLimit(`Chat with ${applicationName ?? ''}`, NAME_MAX_LENGTH),
    is_private: true,
    source: 'agent',
    participants: applicationParticipant ? [applicationParticipant] : [],
    chat_history: getInitialChatHistory(applicationVersionDetails?.welcome_message, activeParticipantId ?? null),
    isNew: true,
    isApplicationChat: true,
  };
}

export function useApplicationChatStreaming(params: UseApplicationChatStreamingParams): UseApplicationChatStreamingResult {
  const {
    socket,
    adapter,
    projectId,
    activeConversation,
    chatHistoryRef,
    setChatHistory,
    onInfo,
    onError,
  } = params;

  const isStreaming = useIsStreaming(activeConversation);

  // Always-latest ref for the fields `onDeleteAllMessages` needs beyond `onStopAll` — keeps that
  // callback's own dependency array under this codebase's §3.5 hook-deps budget (8 max) without
  // sacrificing real memoisation (a plain `[params]` dependency would recreate the callback every
  // render, since the caller passes a fresh object literal each time — this ref avoids that).
  const paramsRef = useRef(params);
  paramsRef.current = params;

  const onStopStreaming = useCallback(
    (message: ChatHistoryMessage) => async () => {
      const { id: streamId, task_id } = message;
      if (task_id && streamId !== undefined) {
        await adapter.stopChatTask({ projectId, messageGroupUuid: streamId });
      }
      // A DIFFERENT room concept from the conversation room (useSocketRoom) — per-stream, matching
      // the baseline's own array-of-ids `chat_leave_rooms` emit shape exactly.
      socket.emit('chat_leave_rooms', [streamId]);
      setTimeout(() => setChatHistory((prev) => prev.map((msg) => clearStreamFields(msg, streamId))), 200);
    },
    [adapter, projectId, setChatHistory, socket],
  );

  /**
   * `useApplicationChat.hooks.js:561-570`'s `messagesWithTaskId.forEach(async message => {...})`.
   *
   * **Confirmed regression fix (A1-application-chat cluster, finding 4):** baseline's `forEach`
   * callback is `async` but the outer function never `await`s the `forEach` call itself, so every
   * `stopChatTask` round-trip fires concurrently and `chat_leave_rooms` is emitted immediately after
   * the (synchronous) dispatch loop — NOT after any of them settle. A prior port of this function
   * `await`ed each `stopChatTask` call inside a `for` loop, making N concurrently-streaming messages
   * (e.g. swarm children) take roughly N times as long to stop as baseline, since `chat_leave_rooms`
   * (and the promise this function returns, which callers like `onDeleteAllMessages` below DO await)
   * only resolved after every network round-trip completed in sequence. Restored to fire-and-parallel:
   * every `stopChatTask` call is dispatched without being awaited (`void`, matching baseline's own
   * unhandled-rejection-on-failure behaviour — baseline's `await` inside the un-awaited `forEach`
   * callback has the exact same "errors are never observed by the caller" shape), and
   * `chat_leave_rooms` fires right after the dispatch loop.
   */
  const onStopAll = useCallback(() => {
    const inFlight = chatHistoryRef.current.filter((message) => message.role !== ROLES.User && isMessageInFlight(message));
    const streamIds = inFlight.map((message) => message.id);

    for (const message of inFlight) {
      if (message.task_id && message.id !== undefined) {
        void adapter.stopChatTask({ projectId, messageGroupUuid: message.id });
      }
    }
    if (streamIds.length) socket.emit('chat_leave_rooms', streamIds);

    setTimeout(
      () => setChatHistory((prev) => prev.map((msg) => ({ ...msg, isStreaming: false, isLoading: false, isRegenerating: false, task_id: undefined }))),
      200,
    );

    return Promise.resolve();
  }, [adapter, projectId, setChatHistory, socket, chatHistoryRef]);

  const onDeleteMessage = useCallback(
    async (messageIdToDelete: string | number, callback?: () => void) => {
      const result = await adapter.deleteMessage({ conversationId: activeConversation?.id, projectId, id: messageIdToDelete });
      if (result.error) {
        onError?.(
          applicationErrorMessageOrFallback(
            result.error,
            t('features.agents.applicationChat.deleteMessageFailed', 'Failed to delete the message, please try again.'),
          ),
        );
        return;
      }
      // Prune every group the SERVER says it removed, not just the one we
      // named. A delete takes the answer AND the question it replies to, so a
      // single-id filter would leave an orphaned question rendered until a
      // reload. `deleted` is absent from an adapter that cannot report it, in
      // which case the requested id is still known to be gone.
      const removed = new Set<string>(result.deleted?.map(String) ?? [String(messageIdToDelete)]);
      setChatHistory((prev) => {
        const updated = prev.filter((msg) => !removed.has(String(msg.id)));
        callback?.();
        return updated;
      });
      onInfo?.('The message has been deleted');
    },
    [activeConversation?.id, adapter, projectId, setChatHistory, onInfo, onError],
  );

  const onDeleteAllMessages = useCallback(
    async (callback?: () => void) => {
      const p = paramsRef.current;
      await onStopAll();
      const result = await p.adapter.deleteAllMessages({ projectId: p.projectId, conversationId: p.activeConversation?.id });
      if (result.error) {
        p.onError?.(
          applicationErrorMessageOrFallback(
            result.error,
            t('features.agents.applicationChat.deleteMessageFailed', 'Failed to delete the message, please try again.'),
          ),
        );
        return;
      }
      p.setActiveConversation(
        buildFreshConversation(p.applicationName, p.applicationParticipant, p.applicationVersionDetails, p.activeParticipantId),
      );
      p.onInfo?.('The messages have been deleted');
      callback?.();
    },
    [onStopAll],
  );

  return { isStreaming, onStopStreaming, onStopAll, onDeleteMessage, onDeleteAllMessages };
}
