/**
 * ChatBox — composition root for the chat experience. Composes entities/conversation
 * lifecycle + streaming, features/chat-messages ChatMessageList, features/chat-input
 * NewChatInput, Phase-2 button primitives, Phase-4 recommendation list, and TTS.
 * Port of the old 2300-line ChatBox.jsx — split across sibling hooks
 * (data/state/handlers/participant/model-selection/internal-tools/
 * versioning/mentions/actions — see ./hooks/) plus a pure-helpers module
 * (./ChatBox.helpers.ts) to stay under the §3.5 file-length/component-props/
 * use-effects/complexity budgets.
 *
 * Lives in widgets/ (not features/) because a composition root that pulls
 * together chat-messages, chat-input, and chat-recommendations necessarily
 * imports sideways across those features — legal for widgets/, forbidden
 * for features/ (R-L1, `.dependency-cruiser.cjs`'s `no-sideways-features`).
 */
import type { ComponentRef, Ref } from 'react';
import { memo, useCallback, useEffect, useImperativeHandle, useRef } from 'react';

import { Box } from '@mui/material';
import { useNavBlockerStore } from '@/widgets/app-shell';
import type { AttachmentButtonHandle, PlusChatButtonEntitySubmenus, VoiceButtonHandle } from '@/widgets/chat';
import { ChatConversationStarters, NewChatInput, voiceHooks } from '@/features/chat-input';
import { useSocketClient } from '@/shared/api/socket/client';
import { ChatMessageList, useDeleteMessageAlert } from '@/features/chat-messages';
import { getAllTokens, McpAuthModal } from '@/features/mcps';
import { conversationApi } from '@/entities/conversation';
import { t } from '@/shared/i18n';

import {
  buildAgentEditorProps,
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
import type { ChatBoxEditorCallbacks } from './ChatBox.helpers';
import type { ChatBoxAgentEventSink, ChatBoxConversationProp } from './ChatBox.props';
import { unwrapChatBoxConversation } from './ChatBox.props';
import type { ChatBoxHandle } from './ChatBox.types';
import { buildChatBoxAttachmentProps, buildChatBoxInputSlots } from './ChatBoxInputSlots';
import { buildChatBoxPopupsProps, ChatBoxPopups } from './ChatBoxPopups';
import { ChatBoxDeleteModal } from './ChatBoxDeleteModal';
import { ChatEmptyGreeting } from './ChatEmptyGreeting';
import { chatColumnSx, chatShellSx } from './ChatBox.layout';
import { useChatBoxData } from './hooks/useChatBoxData';
import { useChatBoxState, type ConversationStarter } from './hooks/useChatBoxState';
import { useChatBoxHandlers } from './hooks/useChatBoxHandlers';
import { useChatBoxParticipant } from './hooks/useChatBoxParticipant';
import { useChatBoxModelSelection } from './hooks/useChatBoxModelSelection';
import { useChatBoxInternalTools } from './hooks/useChatBoxInternalTools';
import { useChatBoxVersioning } from './hooks/useChatBoxVersioning';
import { useChatBoxMentions } from './hooks/useChatBoxMentions';
import { useChatBoxActions } from './hooks/useChatBoxActions';
import { useAddEntityParticipant } from './hooks/useAddEntityParticipant';
import { useActiveParticipantSelection } from './hooks/useActiveParticipantSelection';
import { useSessionMcpAuthorizationRefs } from './hooks/useSessionMcpAuthorizationRefs';
import { useChatBoxSend } from './hooks/useChatBoxSend';
import { useStableRef } from './hooks/useStableRef';

/** `NewChatInputHandle` stays unexported from `features/chat-input`'s barrel — derived via `ComponentRef`, matching that barrel's own documented convention. */
type NewChatInputHandle = ComponentRef<typeof NewChatInput>;

/* ------------------------------------------------------------------ */
/*  Props & handle                                                      */
/* ------------------------------------------------------------------ */

/** @public Props for the ChatBox composition root. */
export interface ChatBoxProps {
  /** Host ref for the `ChatBoxHandle` (React 19 passes `ref` as a prop) — see `ChatBox.types.ts`. */
  readonly ref?: Ref<ChatBoxHandle> | undefined;
  /** Bundled to stay under the §3.5 component-props budget (one slot instead of two), which the `ref` prop above pushed this component over — see `ChatBox.props.ts`. */
  readonly conversation?: ChatBoxConversationProp;
  readonly hidden?: boolean;
  readonly fromTheChat?: boolean;
  readonly projectId?: string | number;
  /** Bundled to stay under the §3.5 component-props budget (one slot instead of three). */
  readonly user?: { readonly id?: string; readonly name?: string; readonly avatar?: string };
  /** Bundled to stay under the §3.5 component-props budget (one slot instead of two). */
  readonly participant?: { readonly active?: unknown; readonly onChange?: (participant: unknown) => void };
  readonly setChatHistory?: React.Dispatch<React.SetStateAction<readonly unknown[]>>;
  readonly conversationStarters?: readonly ConversationStarter[];
  readonly isAgentsPage?: boolean;
  /** Bundled to stay under the §3.5 component-props budget (one slot instead of two). */
  readonly llm?: { readonly settings?: Readonly<Record<string, unknown>>; readonly onSetSettings?: (settings: Readonly<Record<string, unknown>>) => void };
  /** Bundled to stay under the §3.5 component-props budget (one slot instead of two). */
  readonly onDelete?: { readonly answer?: (messageId: string) => void; readonly all?: () => void };
  /** Host-supplied composer extension points, bundled to stay under the §3.5 component-props budget (one slot instead of two, as `onDelete` above); both pass straight through. */
  readonly extensions?: {
    /** Agent/pipeline editor open/close callbacks — see `ChatBox.helpers.ts`'s `buildAgentEditorProps`. Optional; falls back to the pre-existing no-ops. */
    readonly editorCallbacks?: ChatBoxEditorCallbacks;
    /** Real lists for the composer's "+" menu — see `processes/chat/model/usePlusMenuEntities.ts`, which is the only layer allowed to fetch them. */
    readonly entitySubmenus?: PlusChatButtonEntitySubmenus;
    readonly onAgentEvent?: ChatBoxAgentEventSink | undefined; // The run of the turn, not only its answer — see `ChatBoxAgentEventSink`.
  };
}

export type { ChatBoxHandle };

/* ------------------------------------------------------------------ */
/*  Component                                                           */
/* ------------------------------------------------------------------ */

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
  const { editorCallbacks, entitySubmenus, onAgentEvent } = extensions ?? {};
  const chatInputRef = useRef<NewChatInputHandle>(null);
  const attachmentButtonRef = useRef<AttachmentButtonHandle>(null); const voiceButtonRef = useRef<VoiceButtonHandle>(null);
  const { activeConversation, isLoadingConversation, onConversationCreated } = unwrapChatBoxConversation(conversation);
  const { userId, userName, userAvatar, llmSettings, onSetLLMSettings, onDeleteAnswer, onDeleteAllMessages } = flattenChatBoxProps({ user, llm, onDelete });
  const { conversationId, conversationParticipants, conversationUuid, conversationMeta, isConversationSending, projectIdString } = deriveChatBoxIds(activeConversation, projectId);
  const { activeParticipant, onChangeParticipant } = useActiveParticipantSelection(participant, conversationParticipants);

  const data = useChatBoxData(buildChatBoxDataParams({ activeConversation, activeParticipant, projectId, userId, userName, userAvatar, isAgentsPage }));
  const messages = data.messageList.messages;

  // Participant normalisation + details fetch
  const { participantForEditor, normalisedParticipants, agentEditorParticipantDetails, isFetchingParticipantDetails, assistantName } = useChatBoxParticipant({
    activeParticipant,
    conversationParticipants,
  });
  const activeParticipantVersions = agentEditorParticipantDetails?.versions;

  // Local state (mentions, slash/skill phases, participant guards, etc.) —
  // called after participant normalisation so its version-missing guard can
  // consume `agentEditorParticipantDetails`'s resolved version list.
  const state = useChatBoxState(buildChatBoxStateParams({
    activeParticipant: participantForEditor,
    participants: normalisedParticipants,
    userId,
    conversationStarters,
    isAgentsPage,
    chatInput: chatInputRef,
    projectId,
    activeParticipantVersions,
  }));

  const socketClient = useSocketClient();
  const readAloud = voiceHooks.useReadAloud({
    projectId: projectIdString,
    socket: socketClient,
  });
  const lifecycle = data.lifecycle;

  // Mirror the live, socket-synced history out to the parent's own mirror,
  // when one is supplied (no live caller exists yet — the routing gap this
  // whole unit operates under).
  useEffect(() => {
    setChatHistory?.(messages);
  }, [messages, setChatHistory]);

  const setStreamingBlockNav = useNavBlockerStore((s) => s.setStreamingBlockNav);
  useEffect(() => {
    setStreamingBlockNav(data.streaming.isStreamingNow, 'prompt');
  }, [data.streaming.isStreamingNow, setStreamingBlockNav]);

  // Session-scoped bookkeeping of MCP servers declined/authenticated this
  // conversation (never persisted) — resets whenever the conversation changes.
  const { sessionDeclinedMcpServersRef, sessionMcpAuthorizationBatchesRef } =
    useSessionMcpAuthorizationRefs(conversationUuid);

  // Real RTK/TanStack mutations the action handlers below trigger.
  const { mutateAsync: regenerateMutateAsync } = conversationApi.useRegenerate();
  const { mutateAsync: deleteMessageMutateAsync } = conversationApi.useDeleteMessage();
  const { mutateAsync: deleteAllMessagesMutateAsync } = conversationApi.useDeleteAllMessages();
  const { mutateAsync: stopChatTaskMutateAsync } = conversationApi.useStopTask();

  // Everything one send needs: the SSE transport (issue #93) plus the
  // create-conversation-first and upload-attachments-first adapters.
  // `startStreamedExecution` reports whether the transport took the run, so
  // `sendQuestion` knows not to ALSO emit `chat_predict`.
  const { startStreamedExecution, continueStreamedExecution, regenerateStreamedExecution, stopStreamedExecution, isStreaming: isStreamedExecution, createConversationForSend, uploadAttachmentsForSend } = useChatBoxSend({
    deps: { createConversation: lifecycle.createConversation, uploadAttachments: data.attachments.upload.uploadAttachments },
    setChatHistory: data.setChatHistory, projectId, projectIdString, isAgentsPage, conversationUuid,
    activeParticipant, participants: conversationParticipants, userName, userAvatar,
    llmSettings, model: data.selectedModel, userId, onAgentEvent,
  });
  // After `useChatBoxSend`: a "+" pick on a chat with no conversation has to create one first, and it reuses the adapter the first send would have used, so an eagerly created conversation is seeded exactly like a send-created one.
  const entityParticipantActions = useAddEntityParticipant({ projectId, conversationId, participants: normalisedParticipants, onChangeParticipant, createConversation: () => createConversationForSend(''), ...(onConversationCreated ? { onConversationCreated } : {}) });
  // `isStreamingNow` is derived from the PERSISTED message groups, which carry
  // no in-flight flag while an SSE turn runs — without the transport's own flag
  // the composer never offers Stop for the very turn Stop exists to cancel (#328).
  const isStreaming = [data.streaming.isStreamingNow, data.messageList.isStreamingFromHistory, isStreamedExecution].some(Boolean);

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
    uploadAttachments: uploadAttachmentsForSend,
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
    sessionMcpAuthorizationBatchesRef,
    getMcpTokens: getAllTokens,
    startStreamedExecution, continueStreamedExecution, regenerateStreamedExecution,
  });

  // Delete confirmation (single message / clear-all) — baseline:
  // `useDeleteMessageAlert.hooks.js`'s confirm-then-delete gating.
  const deleteAlert = useDeleteMessageAlert({
    onDeleteChatMessage: async (messageId) => {
      await handlers.deleteAnswer(messageId);
      onDeleteAnswer?.(messageId);
    },
    onDeleteAllChatMessages: async () => {
      await handlers.clearChat();
      onDeleteAllMessages?.();
    },
  });

  // LLM model list + selection
  const { modelsList, selectedLlmModel, handleSelectModel } = useChatBoxModelSelection({
    projectId,
    selectedModelName: data.selectedModel?.name,
    setSelectedModel: data.setSelectedModel,
  });

  const { internalToolsButtonTools, handleInternalToolChange, isUpdatingInternalToolsConfig } = useChatBoxInternalTools({
    conversationId,
    conversationMeta,
    projectId,
    isAgentsPage,
  });

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

  // "@" mention -> send-to-user/everyone routing, "~" skill selection
  const { handleMentionChange, handleSelectUserMention, handleSelectSkillTool } = useChatBoxMentions({ state, onChangeParticipant });

  // Input disable/loading derivation
  const { isInputLoading, disabledSend } = deriveChatBoxInputState({
    isLoadingConversation,
    isFetchingParticipantDetails,
    isUploadingAttachments: data.attachments.upload.isUploading,
    isUpdatingInternalToolsConfig,
    isConversationSending,
    isStreaming,
    hasChatInput: !!chatInputRef.current,
    isProcessingSymbols: state.keyDown.isProcessingSymbols,
    hasPendingHitlInterrupt: data.hasPendingHitlInterrupt,
    isActiveParticipantBroken: state.isActiveParticipantBroken,
  });

  // Action callbacks (send, regenerate, copy, delete, edit-resubmit, HITL
  // resume, MCP/token-limit continue, clear chat, conversation-starter send)
  const readAloudRef = useStableRef(readAloud);
  const readAloudStop = useCallback(() => { readAloudRef.current.stop(); }, [readAloudRef]);
  const {
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
  } = useChatBoxActions({ chatInputRef, data, state, handlers, deleteAlert, messages, isAgentsPage, readAloudStop, onConversationCreated });

  // Imperative handle (stable via refs, so identity never churns)
  const handleClearRef = useStableRef(handleClear);
  const streamingRef = useStableRef(data.streaming);
  // Stop has two halves and needs both (#328): the socket-era stop-task + room
  // leave, for a task the SSE path never registered, and the transport's own
  // cancel-and-close, for a stream the socket path never opened.
  const stopStreamRef = useStableRef(stopStreamedExecution);
  const stopGeneration = useCallback(() => {
    stopStreamRef.current();
    streamingRef.current.stopStreaming();
  }, [stopStreamRef, streamingRef]);
  // MUST target the host `ref`, never `chatInputRef` — see `ChatBox.types.ts`.
  useImperativeHandle(ref, () => ({
    onClear: () => { handleClearRef.current(); },
    mentionUser: (c) => { chatInputRef.current?.setValue?.(`@${c} `); },
    stopAll: stopGeneration,
  }), [handleClearRef, stopGeneration]);

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
          assistantName={assistantName} emptyState={<ChatEmptyGreeting userName={userName} />}
          chatHistory={messages} isStreaming={isStreaming} userId={userId ?? ''} projectId={projectIdString}
          messageActions={{
            onCopyToClipboard: handleCopy,
            onDeleteAnswer: handleDeleteAnswer,
            onRegenerateAnswer: handleRegenerate,
            onSubmitEditedMessage: handleSubmitEditedMessage,
          }}
          continuation={{
            onHitlResume: handleHitlResume,
            onContinueMcpExecution: handleContinueMcpExecution,
            onContinueTokenLimitExecution: handleContinueTokenLimit,
            renderAuthModal: (props) => <McpAuthModal {...props} projectId={projectIdString} />,
          }}
          tts={buildTtsProps(readAloud)}
        />
        {state.shouldShowStarters && (
          <ChatConversationStarters
            onSend={handleSendStarter}
            conversationStarters={resolveConversationStarters(state.hasStarterBeenSent, messages.length, conversationStarters)}
          />
        )}
      </Box>
      <Box sx={{ p: 1 }}>
        <ChatBoxPopups
          {...buildChatBoxPopupsProps({
            state,
            onChangeParticipant,
            existingParticipants: conversationParticipants ?? [],
            projectId: projectIdString,
            onSelectUser: handleSelectUserMention,
            onSelectTool: handleSelectSkillTool,
          })}
        />
        <NewChatInput
          ref={chatInputRef}
          conversationId={conversationId !== undefined ? String(conversationId) : undefined}
          state={{ isLoading: isInputLoading, isStreaming, disabledSend, isCreatingConversation: data.lifecycle.isCreating }}
          content={{ placeholder: t('widgets.chatBox.inputPlaceholder', 'Type your message...'), clearInputAfterSubmit: true, slashHighlights: state.combinedHighlightRanges }}
          callbacks={{ onSend: handleSend, onStopGeneration: stopGeneration, onNormalKeyDown: state.onNormalKeyDown, onInputChange: state.onInputChange }}
          agentEditor={buildAgentEditorProps({
            participantForEditor,
            activeParticipantDetails: agentEditorParticipantDetails,
            isAgentsPage,
            selectSavedOrDefaultModel: data.selectSavedOrDefaultModel,
            onShowParticipantsList: () => state.setShowRecommendationList(!state.showRecommendationList),
            onSelectVersion: (version) => { void handleSelectVersion(version); },
            editorCallbacks,
          })}
          attachments={buildChatBoxAttachmentProps(data.attachments)}
          mentions={{ users: state.users, onMentionChange: handleMentionChange }}
          voice={{ isSpeakingMode: state.isSpeakingMode, onSpeakingModeToggle: () => state.setIsSpeakingMode(!state.isSpeakingMode), isTTSPlaying: readAloud.isPlaying }}
          slots={buildChatBoxInputSlots({
            attachments: { attachments: data.attachments.state.attachments, onAttachFiles: data.attachments.state.onAttachFiles },
            internalTools: { disabled: isInputLoading, tools: internalToolsButtonTools, onToolChange: handleInternalToolChange },
            model: { llmSettings, onSetLLMSettings, selectedModel: selectedLlmModel, onSelectModel: handleSelectModel, models: modelsList },
            refs: { attachmentButtonRef, voiceButtonRef, voiceInputRef: chatInputRef },
            isAgentsPage: !!isAgentsPage,
            entitySubmenus: { ...entitySubmenus, onSelectParticipant: entityParticipantActions.onSelectParticipant, getParticipantMenuState: entityParticipantActions.getParticipantMenuState },
            participants: normalisedParticipants,
          })}
          refs={{ attachmentButtonRef, voiceButtonRef }}
        />
      </Box>
      <ChatBoxDeleteModal alert={deleteAlert} />
    </Box>
  );
});

const ChatBox = memo(ChatBoxInner);
ChatBox.displayName = 'ChatBox';

export default ChatBox;
