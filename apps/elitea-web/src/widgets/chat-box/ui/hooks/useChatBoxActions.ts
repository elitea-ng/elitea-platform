/**
 * Split out of `ChatBox.tsx` to stay under the file-length/component-props/
 * complexity budgets (§3.5) — the stable action-callback bundle ChatBox's
 * render passes down to `ChatMessageList`/`NewChatInput` (send, regenerate,
 * copy, delete, edit-and-resubmit, HITL resume, MCP/token-limit continue,
 * clear chat, conversation-starter send).
 */
import type { RefObject } from 'react';
import { useCallback } from 'react';

import type { ChatMessage, useDeleteMessageAlert } from '@/features/chat-messages';

import { deriveHitlChildThreadId } from '../ChatBox.helpers';
import { useStableRef } from './useStableRef';
import type { useChatBoxData } from './useChatBoxData';
import type { useChatBoxHandlers } from './useChatBoxHandlers';
import type { useChatBoxState } from './useChatBoxState';

interface ChatInputHandleLike {
  reset?: () => void;
  setValue?: (value: string) => void;
}

export interface UseChatBoxActionsParams {
  readonly chatInputRef: RefObject<ChatInputHandleLike | null>;
  readonly data: ReturnType<typeof useChatBoxData>;
  readonly state: ReturnType<typeof useChatBoxState>;
  readonly handlers: ReturnType<typeof useChatBoxHandlers>;
  readonly deleteAlert: ReturnType<typeof useDeleteMessageAlert>;
  readonly messages: readonly ChatMessage[];
  readonly isAgentsPage: boolean | undefined;
  readonly readAloudStop: () => void;
  readonly onConversationCreated?: ((conversation: { readonly id?: string | number; readonly uuid?: string }) => void) | undefined;
}

export interface UseChatBoxActionsResult {
  readonly handleSend: (question: string) => void;
  readonly handleSendStarter: (starter: string) => void;
  readonly handleRegenerate: (messageId: string) => void;
  readonly handleCopy: (message: ChatMessage) => void;
  readonly handleDeleteAnswer: (messageId: string) => void;
  readonly handleSubmitEditedMessage: (messageId: string, updatedItems: readonly { uuid?: string | undefined; content: string; item_type: string }[]) => void;
  readonly handleHitlResume: (payload: { action: 'approve' | 'reject' | 'edit' | 'block_with_comment' | 'answer'; value?: string | undefined; toolCallId?: string | undefined }) => void;
  readonly handleContinueMcpExecution: (messageId: string, addToIgnoreList?: boolean, authorizationRequestId?: string) => void;
  readonly handleContinueTokenLimit: (messageId: string) => void;
  readonly handleClear: () => void;
}

export function useChatBoxActions({
  chatInputRef,
  data,
  state,
  handlers,
  deleteAlert,
  messages,
  isAgentsPage,
  readAloudStop,
  onConversationCreated,
}: UseChatBoxActionsParams): UseChatBoxActionsResult {
  const handleSend = useCallback(
    (question: string) => {
      if (!question.trim() || data.hasPendingHitlInterrupt || state.isActiveParticipantBroken) return;
      const isSendingToUser = state.isMentioningEveryone || state.selectedUsers.length > 0;
      const userIds = state.isMentioningEveryone
        ? state.users.filter((u) => u.id !== '@everyone').map((u) => u.id)
        : state.selectedUsers.map((u) => u.id);
      state.setIsMentioningEveryone(false);
      state.setSelectedUsers([]);
      state.slash.resetSlash();
      chatInputRef.current?.reset?.();
      const pendingAttachments = data.attachments.state.attachments;
      data.attachments.state.onClearAttachments();
      void handlers.sendQuestion({ question, attachments: pendingAttachments, isSendingToUser, userIds }).then((result) => {
        // Announced on `result.createdConversation` alone, NOT on `success`:
        // the row is committed before any transport is tried, so a turn that
        // then fails still leaves a conversation the route and the rail have
        // to learn about (see `SendResult`).
        if (result.createdConversation) onConversationCreated?.(result.createdConversation);
      });
    },
    [data.hasPendingHitlInterrupt, data.attachments.state, handlers, state, chatInputRef, onConversationCreated],
  );

  const handleSendStarter = useCallback(
    (starter: string) => {
      state.setHasStarterBeenSent(true);
      chatInputRef.current?.reset?.();
      chatInputRef.current?.setValue?.(starter);
    },
    [state, chatInputRef],
  );

  const handleRegenerate = useCallback(
    (messageId: string) => { void handlers.regenerateAnswer(messageId); },
    [handlers],
  );

  const handleCopy = useCallback(
    (message: ChatMessage) => { void handlers.copyToClipboard(message); },
    [handlers],
  );

  const handleDeleteAnswer = useCallback(
    (messageId: string) => { deleteAlert.openDialog(messageId); },
    [deleteAlert],
  );

  const handleSubmitEditedMessage = useCallback(
    (messageId: string, updatedItems: readonly { uuid?: string | undefined; content: string; item_type: string }[]) => {
      const newContent = updatedItems.find((item) => item.item_type === 'text_message')?.content ?? '';
      if (!newContent.trim()) return;
      data.setChatHistory((prev) => prev.map((item) => (item.id !== messageId ? item : { ...item, content: newContent })));
      const answer = messages.find((item) => item.questionId === messageId);
      if (answer) {
        void handlers.regenerateAnswer(answer.id, updatedItems);
      } else {
        void handlers.sendQuestion({ question: newContent });
      }
    },
    [data, messages, handlers],
  );

  const handleHitlResume = useCallback(
    (payload: { action: 'approve' | 'reject' | 'edit' | 'block_with_comment' | 'answer'; value?: string | undefined; toolCallId?: string | undefined }) => {
      const childThreadId = deriveHitlChildThreadId(data.pendingHitlMessage, payload.toolCallId);
      void handlers.continueHitl({
        action: payload.action,
        ...(payload.value !== undefined ? { value: payload.value } : {}),
        ...(payload.toolCallId !== undefined ? { toolCallId: payload.toolCallId } : {}),
        ...(childThreadId !== undefined ? { childThreadId } : {}),
      });
    },
    [data.pendingHitlMessage, handlers],
  );

  const handleContinueMcpExecution = useCallback(
    (messageId: string, addToIgnoreList?: boolean, authorizationRequestId?: string) => {
      void handlers.resumeMcpFlow(messageId, addToIgnoreList, authorizationRequestId);
    },
    [handlers],
  );

  const handleContinueTokenLimit = useCallback(
    (messageId: string) => { void handlers.continueTokenLimit(messageId); },
    [handlers],
  );

  const streamingRef = useStableRef(data.streaming);
  const handleClear = useCallback(() => {
    readAloudStop();
    if (isAgentsPage) {
      // Agents-page reset is purely local — baseline never issues a network
      // call or confirmation dialog here (`ChatBox.jsx:649-695`).
      data.setChatHistory([]);
      streamingRef.current.clearConversationStreamingInfo();
      chatInputRef.current?.reset?.();
      return;
    }
    if (messages.length) deleteAlert.openDialogForAll();
  }, [isAgentsPage, data, deleteAlert, messages.length, readAloudStop, chatInputRef, streamingRef]);

  return {
    handleSend,
    handleSendStarter,
    handleRegenerate,
    handleCopy,
    handleDeleteAnswer,
    handleSubmitEditedMessage,
    handleHitlResume,
    handleContinueMcpExecution,
    handleContinueTokenLimit,
    handleClear,
  };
}
