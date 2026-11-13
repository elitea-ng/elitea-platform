/**
 * ChatBox — composition root for the chat experience. Composes entities/conversation
 * lifecycle + streaming, features/chat-messages ChatMessageList, features/chat-input
 * NewChatInput, Phase-2 button primitives, Phase-4 recommendation list, and TTS.
 * Sibling hooks and ChatBox.helpers.ts keep this composition within §3.5 budgets;
 * the composer block is `ChatBoxComposer` and its hook wiring `useChatBoxComposer`.
 *
 * Lives in widgets/ (not features/) because a composition root that pulls
 * together chat-messages, chat-input, and chat-recommendations necessarily
 * imports sideways across those features — legal for widgets/, forbidden
 * for features/ (R-L1, `.dependency-cruiser.cjs`'s `no-sideways-features`).
 */
import type { ComponentRef } from 'react';
import { memo, useEffect, useRef } from 'react';

import { Box } from '@mui/material';
import { useNavBlockerStore } from '@/widgets/app-shell';
import type { AttachmentButtonHandle, VoiceButtonHandle } from '@/widgets/chat';
import { ChatConversationStarters, type NewChatInput, voiceHooks } from '@/features/chat-input';
import { useSocketClient } from '@/shared/api/socket/client';
import { ChatMessageList, currentNodeRecoveryBinding, applySteeringEvent } from '@/features/chat-messages';
import { getAllTokens } from '@/features/mcps';
import { conversationApi } from '@/entities/conversation';
import { WELCOME_MESSAGE_ID } from '@/shared/lib/enums';
import { CONVERSATION_COLUMN } from '@/shared/ui/conversation-column';

import {
  buildCanvasProps,
  buildChatBoxDataParams,
  buildChatBoxHandlerConversationDeps,
  buildChatBoxStateParams,
  buildTtsProps,
  buildUserParticipant,
  deriveChatBoxIds,
  deriveChatBoxInputState,
  flattenChatBoxProps,
  resolveConversationStarters,
} from './ChatBox.helpers';
import { buildMessageActions } from './ChatBox.messageActions';
import { withDraftCarry } from './ChatBox.draft';
import { unwrapChatBoxConversation } from './ChatBox.props';
import type { ChatBoxHandle, ChatBoxProps } from './ChatBox.types';
export type { ChatBoxProps } from './ChatBox.types';
import { buildChatBoxContinuationProps } from './ChatBoxContinuation';
import { ChatBoxComposer } from './ChatBoxComposer';
import { useChatBoxLlmSettingsDialog } from './ChatBoxLlmSettingsDialog';
import { useChatBoxVoiceFeedback } from './ChatBoxVoiceFeedback';
import { ChatBoxWithdrawnNotice } from './ChatBoxWithdrawnNotice';
import { ChatBoxShareHistoryDialog } from './ChatBoxShareHistoryDialog';
import { ChatBoxDeleteModal } from './ChatBoxDeleteModal';
import { ChatEmptyGreeting } from './ChatEmptyGreeting';
import { chatColumnSx, chatShellSx } from './ChatBox.layout';
import { useChatBoxUnattended } from './hooks/useChatBoxUnattended';
import { useChatBoxComposer } from './hooks/useChatBoxComposer';
import { useChatBoxData } from './hooks/useChatBoxData';
import { useChatBoxState } from './hooks/useChatBoxState';
import { useChatBoxHandlers } from './hooks/useChatBoxHandlers';
import { useChatBoxParticipant } from './hooks/useChatBoxParticipant';
import { useResolvedTarget } from './hooks/useResolvedTarget';
import { useChatBoxModelSelection } from './hooks/useChatBoxModelSelection';
import { useChatBoxInternalTools } from './hooks/useChatBoxInternalTools';
import { useChatBoxVersioning } from './hooks/useChatBoxVersioning';
import { useAddEntityParticipant } from './hooks/useAddEntityParticipant';
import { useActiveParticipantSelection } from './hooks/useActiveParticipantSelection';
import { useSessionMcpAuthorizationRefs } from './hooks/useSessionMcpAuthorizationRefs';
import { useChatBoxSend } from './hooks/useChatBoxSend';
import { useChatBoxDeleteAlert } from './hooks/useChatBoxDeleteAlert';
import { useChatBoxInterruptLedger } from './hooks/useChatBoxInterruptLedger';
import { useTurnActivity } from './hooks/useTurnActivity';

/** `NewChatInputHandle` stays unexported from `features/chat-input`'s barrel — derived via `ComponentRef`, matching that barrel's own documented convention. */
type NewChatInputHandle = ComponentRef<typeof NewChatInput>;

export type { ChatBoxHandle };

const ChatBoxInner = memo(function ChatBox({
  ref,
  conversation,
  hidden = false,
  projectId,
  user,
  participant,
  setChatHistory,
  conversationStarters,
  isAgentsPage,
  llm,
  onDelete,
  extensions,
}: ChatBoxProps) {
  const { editorTest, editorCallbacks, entitySubmenus, onAgentEvent, contextIndicator, readOnlyNotice, noRegenerate, onForkFromMessage } = extensions ?? {};
  const chatInputRef = useRef<NewChatInputHandle>(null);
  const attachmentButtonRef = useRef<AttachmentButtonHandle>(null);
  const voiceButtonRef = useRef<VoiceButtonHandle>(null);
  const { activeConversation, isLoadingConversation, onConversationCreated: onCreated, onTurn } = unwrapChatBoxConversation(conversation);
  const onConversationCreated = withDraftCarry(onCreated, { projectId, scratch: [isAgentsPage, editorTest], text: () => chatInputRef.current?.getInputContent() ?? '' });
  const { userId, userName, userAvatar, llmSettings, onSetLLMSettings, onDeleteAnswer, onDeleteAllMessages } = flattenChatBoxProps({ user, llm, onDelete });
  const { conversationId, conversationParticipants, conversationUuid, conversationMeta, isConversationSending, projectIdString } = deriveChatBoxIds(activeConversation, projectId);
  const { activeParticipant, onChangeParticipant } = useActiveParticipantSelection(participant, conversationParticipants);

  const data = useChatBoxData(buildChatBoxDataParams({ activeConversation, activeParticipant, projectId, userId, userName, userAvatar, isAgentsPage }));
  const messages = data.messageList.messages;
  // ADR-0050 D7c: who an unaddressed message goes to, derived once for the send, the stream identity, the attachments gate and "To:".
  const target = useResolvedTarget(activeParticipant, conversationParticipants, messages);
  const { participantForEditor, normalisedParticipants, agentEditorParticipantDetails, isFetchingParticipantDetails, assistantName, areAttachmentsGated, grantableTools, welcomeMessage, participantStarters } = useChatBoxParticipant({ activeParticipant, conversationParticipants, target, isAgentsPage });
  const activeParticipantVersions = agentEditorParticipantDetails?.versions;
  // #1988: the bound agent's own starters, unless the caller supplied a list (the editor's test pane passes its live form).
  const effectiveStarters = conversationStarters ?? participantStarters;
  // ADR-0045: "Run to completion", on by default: the grant each send carries.
  const unattended = useChatBoxUnattended(conversationUuid, grantableTools, !editorTest);

  // Local state (mentions, slash/skill phases, participant guards, etc.) —
  // called after participant normalisation so its version-missing guard can
  // consume `agentEditorParticipantDetails`'s resolved version list.
  const state = useChatBoxState(buildChatBoxStateParams({
    activeParticipant: participantForEditor,
    participants: normalisedParticipants,
    userId,
    conversationStarters: effectiveStarters,
    isAgentsPage,
    chatInput: chatInputRef,
    projectId,
    activeParticipantVersions,
  }));

  // Socket client + read-aloud (TTS)
  const socketClient = useSocketClient();
  // #932/#934: VoiceButton's recording flag + its error messages, which had no caller at all
  const voiceFeedback = useChatBoxVoiceFeedback();
  const readAloud = voiceHooks.useReadAloud({ projectId: projectIdString, onError: voiceFeedback.onError }); // read-aloud failures share the dictation error toast
  const lifecycle = data.lifecycle;

  // Mirror the live, socket-synced history out to the parent's own mirror, when one
  // is supplied (no live caller exists yet — the routing gap this unit operates under).
  useEffect(() => {
    setChatHistory?.(messages);
  }, [messages, setChatHistory]);

  const setStreamingBlockNav = useNavBlockerStore((s) => s.setStreamingBlockNav);
  useEffect(() => {
    setStreamingBlockNav(data.streaming.isStreamingNow, 'prompt');
  }, [data.streaming.isStreamingNow, setStreamingBlockNav]);

  // Session-scoped bookkeeping of MCP servers declined/authenticated this
  // conversation (never persisted) — resets whenever the conversation changes.
  const { sessionDeclinedMcpServersRef } =
    useSessionMcpAuthorizationRefs(conversationUuid);

  // Real RTK/TanStack mutations the action handlers below trigger.
  const { mutateAsync: regenerateMutateAsync } = conversationApi.useRegenerate();
  const { mutateAsync: deleteMessageMutateAsync } = conversationApi.useDeleteMessage();
  const { mutateAsync: deleteAllMessagesMutateAsync } = conversationApi.useDeleteAllMessages();
  const { mutateAsync: stopChatTaskMutateAsync } = conversationApi.useStopTask();

  const internalTools = useChatBoxInternalTools({
    conversationId,
    conversationMeta,
    projectId,
    isAgentsPage,
  });
  const { isUpdatingInternalToolsConfig, getInternalToolsForSend } = internalTools;

  // Everything one send needs: the SSE transport (issue #93) plus the
  // create-conversation-first and upload-attachments-first adapters.
  // `startStreamedExecution` reports whether the transport took the run, so
  // `sendQuestion` knows not to ALSO emit `chat_predict`.
  // LLM model list + selection. Before the send: a configured model saved with
  // no project id runs in the project the picker resolved.
  const modelSelection = useChatBoxModelSelection({
    projectId,
    selectedModel: data.selectedModel,
    llm,
    setSelectedModel: data.setSelectedModel,
  });
  const { configuredModelProjectId } = modelSelection;
  const sendTransport = useChatBoxSend({
    deps: { createConversation: lifecycle.createConversation, uploadAttachments: data.attachments.upload.uploadAttachments },
    setChatHistory: data.setChatHistory,
    projectId,
    projectIdString,
    isAgentsPage,
    conversationUuid,
    target,
    participants: conversationParticipants,
    userName,
    userAvatar,
    llmSettings,
    model: data.selectedModel,
    configuredModelProjectId,
    userId,
    onAgentEvent,
    getInternalToolsForSend,
    editorTest,
    onSteerEvent: applySteeringEvent,
    unattended: unattended.wire,
  });
  const { startStreamedExecution, continueStreamedExecution, regenerateStreamedExecution, reattachStreamedExecution, createConversationForSend } = sendTransport;
  // Per-interrupt cards across tabs; a fan-out wake attaches.
  useChatBoxInterruptLedger({ projectId, conversationUuid, messages, setChatHistory: data.setChatHistory, reattach: reattachStreamedExecution });
  // After `useChatBoxSend`: a "+" pick on a chat with no conversation has to create one first, and it reuses the adapter the first send would have used, so an eagerly created conversation is seeded exactly like a send-created one.
  const entityParticipantActions = useAddEntityParticipant({
    projectId,
    conversationId,
    participants: normalisedParticipants,
    onChangeParticipant,
    createConversation: () => createConversationForSend(''),
    onConversationCreated,
    // the client-only welcome row is not history
    hasMessages: messages.some((message) => message.id !== WELCOME_MESSAGE_ID),
  });
  // `isStreamingNow` is derived from the PERSISTED message groups, which carry no
  // in-flight flag while an SSE turn runs — without the transport's own flag the
  // composer never offers Stop for the turn Stop exists to cancel (#328).
  const isStreaming = [data.streaming.isStreamingNow, data.messageList.isStreamingFromHistory, sendTransport.isStreaming].some(Boolean);
  useTurnActivity(isStreaming, conversationId, onTurn); // the conversation lists move it to Today

  // Action handlers — real socket protocol (chat_predict / chat_continue_predict),
  // real REST mutations, real conversation-creation-first send ordering.
  const handlers = useChatBoxHandlers({
    // eslint-disable-next-line typescript/unbound-method -- `emit` is a plain closure over `socket` (shared/api/socket/client.ts), never reads `this`
    emitSocket: socketClient.emit,
    chatHistory: messages,
    setChatHistory: data.setChatHistory,
    isStreamingNow: data.streaming.isStreamingNow,
    setStreamingInfo: data.streaming.setStreamingInfo,
    createConversation: createConversationForSend,
    uploadAttachments: sendTransport.uploadAttachmentsForSend,
    triggerRegenerate: regenerateMutateAsync,
    triggerDeleteMessage: deleteMessageMutateAsync,
    triggerDeleteAllMessages: deleteAllMessagesMutateAsync,
    triggerStopChatTask: stopChatTaskMutateAsync,
    getUserParticipant: () => buildUserParticipant(userId, userName, userAvatar),
    getActiveParticipant: () => activeParticipant,
    ...buildChatBoxHandlerConversationDeps(activeConversation),
    projectId,
    socketId: socketClient.socket.id,
    sessionDeclinedMcpServersRef,
    getMcpTokens: getAllTokens,
    startStreamedExecution,
    continueStreamedExecution,
    regenerateStreamedExecution,
  });

  // Delete confirmation (single message / clear-all).
  const deleteAlert = useChatBoxDeleteAlert(handlers, onDeleteAnswer, onDeleteAllMessages);

  // Version selection (real fetch + persist) + auto-recovery
  const { handleSelectVersion } = useChatBoxVersioning({
    participantForEditor,
    projectId,
    activeConversationId: conversationId,
    activeParticipant,
    onChangeParticipant,
    isActiveParticipantVersionMissing: state.isActiveParticipantVersionMissing,
    activeParticipantVersions,
  });

  // A14 (ELITEA-0386): per-participant LLM-settings edit dialog
  const llmSettingsDialog = useChatBoxLlmSettingsDialog({ projectId: projectIdString, conversationId, participant: participantForEditor });

  // Input disable/loading derivation
  const inputState = deriveChatBoxInputState({
    isLoadingConversation,
    isEditorTest: Boolean(editorTest),
    activeConversation,
    isFetchingParticipantDetails,
    isUploadingAttachments: data.attachments.upload.isUploading,
    isUpdatingInternalToolsConfig,
    isConversationSending,
    isStreaming,
    hasChatInput: !!chatInputRef.current,
    hasPendingHitlInterrupt: data.hasPendingHitlInterrupt,
    hasPendingNodeRecovery: Boolean(currentNodeRecoveryBinding(messages)),
    isActiveParticipantBroken: state.isActiveParticipantBroken,
    isActiveParticipantWithdrawn: state.isActiveParticipantWithdrawn,
  });

  const areAttachmentsDisabled = [isStreaming, areAttachmentsGated, [editorTest, inputState.isInputLoading].every(Boolean)].some(Boolean);

  const composer = useChatBoxComposer({
    ref,
    chatInputRef,
    hidden,
    state,
    data,
    handlers,
    deleteAlert,
    messages,
    onChangeParticipant,
    participants: conversationParticipants,
    isAgentsPage,
    isStreaming,
    projectId,
    conversationId,
    conversationUuid,
    editorTest,
    readAloud,
    onConversationCreated,
    onSkillsUnavailable: voiceFeedback.onError,
    send: sendTransport,
    unattended,
  });
  const { handleSendStarter, handleRegenerate, handleCopy, handleDeleteAnswer, handleSubmitEditedMessage, handleHitlResume, handleContinueMcpExecution, handleContinueTokenLimit } = composer.actions;

  // Early return
  if (hidden) return null;

  // Empty-conversation layout mode — see `ChatBox.layout.ts`. Starters count
  // as content: when the agent offers them the column is not visually empty,
  // so the normal top-anchored layout stays.
  const isEmptyConversation = messages.length === 0 && !state.shouldShowStarters;

  return (
    <Box sx={chatShellSx(isEmptyConversation)}>
      <Box sx={chatColumnSx(isEmptyConversation)}>
        <ChatMessageList
          assistant={{ name: assistantName, conversationUuid }}
          emptyState={<ChatEmptyGreeting userName={userName} welcomeMessage={welcomeMessage} />}
          chatHistory={composer.queued.decorate(messages)}
          isStreaming={isStreaming}
          userId={userId ?? ''}
          projectId={projectIdString}
          messageActions={buildMessageActions(readOnlyNotice !== undefined, {
            onCopyToClipboard: handleCopy,
            onDeleteAnswer: handleDeleteAnswer,
            onRegenerateAnswer: handleRegenerate,
            onSubmitEditedMessage: handleSubmitEditedMessage,
            onForkFromMessage,
          }, noRegenerate)}
          continuation={{ ...buildChatBoxContinuationProps({ onHitlResume: handleHitlResume, onContinueMcpExecution: handleContinueMcpExecution, onContinueTokenLimitExecution: handleContinueTokenLimit }, projectIdString), staticPause: composer.staticPause }}
          tts={buildTtsProps(readAloud, state.isSpeakingMode)}
          canvas={buildCanvasProps(editorCallbacks)}
          steering
        />
        {state.shouldShowStarters && (
          <ChatConversationStarters
            onSend={handleSendStarter}
            conversationStarters={resolveConversationStarters(state.hasStarterBeenSent, messages.length, effectiveStarters)}
          />
        )}
      </Box>
      <Box sx={{ ...CONVERSATION_COLUMN, paddingTop: 1, paddingBottom: 2 }}>
        {readOnlyNotice ?? (
          <ChatBoxComposer
            chatInputRef={chatInputRef}
            refs={{ attachmentButtonRef, voiceButtonRef }}
            composer={composer}
            state={state}
            data={data}
            conversation={{ projectId, projectIdString, conversationId, conversationMeta, conversationParticipants, isAgentsPage, editorTest, messageCount: messages.length }}
            participant={{ participantForEditor, agentEditorParticipantDetails, normalisedParticipants, target, onChangeParticipant, onSelectVersion: handleSelectVersion, entityActions: entityParticipantActions }}
            inputState={{ ...inputState, isStreaming, areAttachmentsDisabled }}
            media={{ readAloud, voiceFeedback }}
            tools={{ internal: internalTools, model: modelSelection, llmSettings, onSetLLMSettings, onEditLlmSettings: llmSettingsDialog.onEdit, unattended }}
            extensions={{ contextIndicator, editorCallbacks, entitySubmenus }}
          />
        )}
      </Box>
      <ChatBoxWithdrawnNotice withdrawn={state.isActiveParticipantWithdrawn} />
      <ChatBoxDeleteModal alert={deleteAlert} />
      <ChatBoxShareHistoryDialog question={entityParticipantActions.shareHistory} />
      {llmSettingsDialog.dialog}
      {voiceFeedback.alert}
    </Box>
  );
});

const ChatBox = memo(ChatBoxInner);
ChatBox.displayName = 'ChatBox';

export default ChatBox;
