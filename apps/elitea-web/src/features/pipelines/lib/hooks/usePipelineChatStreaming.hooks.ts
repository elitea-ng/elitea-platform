import { useCallback, useMemo, useRef } from 'react';

import { ROLES } from '@/shared/lib/enums';
import type { SocketClient } from '@/shared/api/socket/client';
import type { Participant } from '@/entities/participant';

import { pipelineErrorMessage } from './pipelineErrorMessage';
import { getInitialChatHistory, isMessageInFlight } from './pipelineChat.helpers';
import type { ChatConversationAdapter, ChatConversation, ChatHistoryMessage, ChatPipelineVersionDetails } from './pipelineChat.types';
import { NAME_MAX_LENGTH, clipToLimit } from '@/shared/lib/textLimits';

/**
 * Streaming/delete slice of `usePipelineChat` — `isStreaming` tracking,
 * `onStopStreaming`/`onStopAll`, `onDeleteMessage`/`onDeleteAllMessages`.
 * Split out of `usePipelineChat.hooks.ts` purely to keep every function
 * under this codebase's `complexity`/`max-lines` gates, mirroring
 * `features/agents/lib/hooks/useApplicationChatStreaming.hooks.ts` for the
 * sibling baseline hook. See that file's own doc comment for the disclosed
 * simplification this port carries forward unchanged: the baseline's
 * redundant `streamingState.streamingMessages` `Set` (written, never
 * exposed, always re-derivable from `chat_history` on the next render) is
 * dropped — `isStreaming` is a plain `useMemo` off `chat_history` directly.
 */
export interface UsePipelineChatStreamingParams {
  readonly socket: SocketClient;
  readonly adapter: ChatConversationAdapter;
  readonly projectId: string | undefined;
  readonly pipelineName: string | undefined;
  readonly pipelineVersionDetails: ChatPipelineVersionDetails | undefined;
  readonly pipelineParticipant: Participant | null;
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

export interface UsePipelineChatStreamingResult {
  readonly isStreaming: boolean;
  readonly onStopStreaming: (message: ChatHistoryMessage) => () => Promise<void>;
  readonly onStopAll: () => Promise<void>;
  readonly onDeleteMessage: (messageIdToDelete: string | number, callback?: () => void) => Promise<void>;
  readonly onDeleteAllMessages: (callback?: () => void) => Promise<void>;
}

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

function buildFreshConversation(
  pipelineName: string | undefined,
  pipelineParticipant: Participant | null,
  pipelineVersionDetails: ChatPipelineVersionDetails | undefined,
  activeParticipantId: string | number | undefined,
): ChatConversation {
  return {
    name: clipToLimit(`Chat with ${pipelineName ?? ''}`, NAME_MAX_LENGTH),
    is_private: true,
    source: 'pipeline',
    participants: pipelineParticipant ? [pipelineParticipant] : [],
    chat_history: getInitialChatHistory(pipelineVersionDetails?.welcome_message, activeParticipantId ?? null),
    isNew: true,
    isPipelineChat: true,
  };
}

export function usePipelineChatStreaming(params: UsePipelineChatStreamingParams): UsePipelineChatStreamingResult {
  const { socket, adapter, projectId, activeConversation, chatHistoryRef, setChatHistory, onInfo, onError } = params;

  const isStreaming = useIsStreaming(activeConversation);

  // Always-latest ref for the fields `onDeleteAllMessages` needs beyond `onStopAll` — keeps that
  // callback's own dependency array under this codebase's §3.5 hook-deps budget (8 max) without
  // sacrificing real memoisation (the caller passes a fresh object literal each time).
  const paramsRef = useRef(params);
  paramsRef.current = params;

  const onStopStreaming = useCallback(
    (message: ChatHistoryMessage) => async () => {
      const { id: streamId, task_id } = message;
      if (task_id && streamId !== undefined) {
        await adapter.stopChatTask({ projectId, messageGroupUuid: streamId });
      }
      socket.emit('chat_leave_rooms', [streamId]);
      setTimeout(() => setChatHistory((prev) => prev.map((msg) => clearStreamFields(msg, streamId))), 200);
    },
    [adapter, projectId, setChatHistory, socket],
  );

  const onStopAll = useCallback(async () => {
    const inFlight = chatHistoryRef.current.filter((message) => message.role !== ROLES.User && isMessageInFlight(message));
    const streamIds = inFlight.map((message) => message.id);

    for (const message of inFlight) {
      if (message.task_id && message.id !== undefined) {
        await adapter.stopChatTask({ projectId, messageGroupUuid: message.id });
      }
    }
    if (streamIds.length) socket.emit('chat_leave_rooms', streamIds);

    setTimeout(
      () => setChatHistory((prev) => prev.map((msg) => ({ ...msg, isStreaming: false, isLoading: false, isRegenerating: false, task_id: undefined }))),
      200,
    );
  }, [adapter, projectId, setChatHistory, socket, chatHistoryRef]);

  const onDeleteMessage = useCallback(
    async (messageIdToDelete: string | number, callback?: () => void) => {
      const result = await adapter.deleteMessage({ conversationId: activeConversation?.id, projectId, id: messageIdToDelete });
      if (result.error) {
        onError?.(pipelineErrorMessage(result.error) || 'Failed to delete the message, please try again.');
        return;
      }
      // Prune every group the SERVER says it removed, not just the one we
      // named: a delete takes the answer AND the question it replies to, so a
      // single-id filter leaves an orphaned question rendered until a reload.
      // Same reasoning as the agents hook, which shares this adapter contract.
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
        p.onError?.(pipelineErrorMessage(result.error) || 'Failed to delete the message, please try again.');
        return;
      }
      p.setActiveConversation(
        buildFreshConversation(p.pipelineName, p.pipelineParticipant, p.pipelineVersionDetails, p.activeParticipantId),
      );
      p.onInfo?.('The messages have been deleted');
      callback?.();
    },
    [onStopAll],
  );

  return { isStreaming, onStopStreaming, onStopAll, onDeleteMessage, onDeleteAllMessages };
}
