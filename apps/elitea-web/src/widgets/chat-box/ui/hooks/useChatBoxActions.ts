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
  readonly handleContinueMcpExecution: (messageId: string, addToIgnoreList?: boolean) => void;
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
      // USER ids, not the participant ids the picker is keyed by. The start
      // route parses `user_ids` as `centry.notifications.user_id`; a
      // participant id there names a different person or nobody. A mention
      // whose participant carries no `entity_meta.id` is dropped rather than
      // sent as a participant id — the server would take it at face value.
      const userIds = (state.isMentioningEveryone
        ? state.users.filter((u) => u.id !== '@everyone')
        : state.selectedUsers
      )
        .map((u) => u.userId)
        .filter((id): id is string => typeof id === 'string' && id !== '');
      const isMentioningEveryone = state.isMentioningEveryone;
      state.setIsMentioningEveryone(false);
      state.setSelectedUsers([]);
      state.slash.resetSlash();
      // issue 974: a new question ends the previous answer's read-out, exactly as
      // the reference SPA's `onSendMessage` does. Otherwise the old answer is
      // still being spoken while the new one streams in.
      readAloudStop();
      chatInputRef.current?.reset?.();
      const pendingAttachments = data.attachments.state.attachments;
      data.attachments.state.onClearAttachments();
      void handlers.sendQuestion({ question, attachments: pendingAttachments, isSendingToUser, userIds, isMentioningEveryone }).then((result) => {
        // Announced on `result.createdConversation` alone, NOT on `success`:
        // the row is committed before any transport is tried, so a turn that
        // then fails still leaves a conversation the route and the rail have
        // to learn about (see `SendResult`).
        if (result.createdConversation) onConversationCreated?.(result.createdConversation);
      });
    },
    [
      data.hasPendingHitlInterrupt,
      data.attachments.state,
      handlers,
      state,
      chatInputRef,
      onConversationCreated,
      readAloudStop,
    ],
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
    (messageId: string) => {
      // issue 974: the old answer stops being read the moment it stops being the
      // answer. Without this the voice went on reading text the transcript had
      // already replaced, over the top of the new turn — upstream bug
      // EliteaAI/elitea_issues#4995, and what the reference SPA stops in
      // `onRegenerateAnswer`.
      readAloudStop();
      void handlers.regenerateAnswer(messageId);
    },
    [handlers, readAloudStop],
  );

  const handleCopy = useCallback(
    (message: ChatMessage) => { void handlers.copyToClipboard(message); },
    [handlers],
  );

  const handleDeleteAnswer = useCallback(
    (messageId: string) => {
      // issue 974: same rule as regenerate — an answer that is being deleted must
      // not keep speaking. Stopped when the dialog OPENS rather than on
      // confirm, which is also what `handleClear` does: the read is over
      // either way, and a voice still reading a message the user is being
      // asked about deleting is the defect.
      readAloudStop();
      deleteAlert.openDialog(messageId);
    },
    [deleteAlert, readAloudStop],
  );

  const handleSubmitEditedMessage = useCallback(
    (messageId: string, updatedItems: readonly { uuid?: string | undefined; content: string; item_type: string }[]) => {
      const newContent = updatedItems.find((item) => item.item_type === 'text_message')?.content ?? '';
      if (!newContent.trim()) return;
      // Was anything actually CHANGED? A save that changed nothing is a RETRY
      // of the same question (onetest ELITEA-0540, issue 980), and a retry is an
      // ordinary regeneration: sending `updated_items` for it would ask the
      // platform to rewrite a question into the text it already holds, which
      // the regeneration contract refuses outright
      // (`!emptyJSONArray(body.UpdatedItems)`, api/v2/agentexecution/route.go)
      // — so the retry would 400 for asking for a rewrite nobody wanted.
      const unchanged = messages.find((item) => item.id === messageId)?.content === newContent;
      data.setChatHistory((prev) => prev.map((item) => (item.id !== messageId ? item : { ...item, content: newContent })));
      const answer = messages.find((item) => item.questionId === messageId);
      if (answer) {
        void handlers.regenerateAnswer(answer.id, ...(unchanged ? [] : [updatedItems]));
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
    (messageId: string, addToIgnoreList?: boolean) => { void handlers.resumeMcpFlow(messageId, addToIgnoreList); },
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
