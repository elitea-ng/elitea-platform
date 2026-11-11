/**
 * The chat composer block, split out of `ChatBox.tsx` (issue 1808) to keep the
 * composition root under the §3.5 file-length budget: the read-aloud player,
 * the A17 queue and steering notices, the mention popups, the host's context
 * indicator, "To:" and `NewChatInput` with its props. `ChatBox` renders it in
 * place of a read-only notice; its hook wiring is `useChatBoxComposer`.
 */
import type { ComponentRef, ReactNode, RefObject } from 'react';

import type { AttachmentButtonHandle, PlusChatButtonEntitySubmenus, VoiceButtonHandle } from '@/widgets/chat';
import { NewChatInput, type voiceHooks } from '@/features/chat-input';
import { t } from '@/shared/i18n';

import { buildAgentEditorProps, buildCreateHandlerProps, shouldDisableClearChat, type ChatBoxEditorCallbacks, type deriveChatBoxInputState } from './ChatBox.helpers';
import { chatDraftKey } from './ChatBox.draft';
import { ChatBoxComposerQueue } from './ChatBoxComposerQueue';
import { ChatBoxFloorTarget } from './ChatBoxFloorTarget';
import { buildChatBoxAttachmentProps, buildChatBoxInputSlots } from './ChatBoxInputSlots';
import { buildChatBoxPopupsProps, ChatBoxPopups } from './ChatBoxPopups';
import { ChatBoxSteeringNotice, ChatBoxSteeringOffer } from './ChatBoxQueuedMessages';
import type { ChatBoxVoiceFeedback } from './ChatBoxVoiceFeedback';
import { ChatBoxVoicePlayer } from './ChatBoxVoicePlayer';
import type { useAddEntityParticipant } from './hooks/useAddEntityParticipant';
import type { ChatBoxComposerModel } from './hooks/useChatBoxComposer';
import type { useChatBoxData } from './hooks/useChatBoxData';
import type { useChatBoxInternalTools } from './hooks/useChatBoxInternalTools';
import type { useChatBoxModelSelection } from './hooks/useChatBoxModelSelection';
import type { useChatBoxParticipant } from './hooks/useChatBoxParticipant';
import type { useChatBoxState } from './hooks/useChatBoxState';
import type { ChatBoxUnattended } from './hooks/useChatBoxUnattended';
import type { ResolvedTarget } from './hooks/useResolvedTarget';

type NewChatInputHandle = ComponentRef<typeof NewChatInput>;

interface ChatBoxComposerProps {
  readonly chatInputRef: RefObject<NewChatInputHandle | null>;
  readonly refs: {
    readonly attachmentButtonRef: RefObject<AttachmentButtonHandle | null>;
    readonly voiceButtonRef: RefObject<VoiceButtonHandle | null>;
  };
  readonly composer: ChatBoxComposerModel;
  readonly state: ReturnType<typeof useChatBoxState>;
  readonly data: ReturnType<typeof useChatBoxData>;
  readonly conversation: {
    readonly projectId: string | number | undefined;
    readonly projectIdString: string | undefined;
    readonly conversationId: string | number | undefined;
    readonly conversationMeta: Readonly<Record<string, unknown>> | undefined;
    readonly conversationParticipants: unknown[] | undefined;
    readonly isAgentsPage: boolean | undefined;
    readonly editorTest: unknown;
    readonly messageCount: number;
  };
  readonly participant: Pick<ReturnType<typeof useChatBoxParticipant>, 'participantForEditor' | 'agentEditorParticipantDetails' | 'normalisedParticipants'> & {
    readonly target: ResolvedTarget;
    readonly onChangeParticipant: (participant: unknown) => void;
    readonly onSelectVersion: (version: Parameters<NonNullable<ReturnType<typeof buildAgentEditorProps>['onSelectVersion']>>[0]) => Promise<unknown>;
    readonly entityActions: ReturnType<typeof useAddEntityParticipant>;
  };
  readonly inputState: ReturnType<typeof deriveChatBoxInputState> & {
    readonly isStreaming: boolean;
    readonly areAttachmentsDisabled: boolean;
  };
  readonly media: {
    readonly readAloud: ReturnType<typeof voiceHooks.useReadAloud>;
    readonly voiceFeedback: ChatBoxVoiceFeedback;
  };
  readonly tools: {
    readonly internal: ReturnType<typeof useChatBoxInternalTools>;
    readonly model: ReturnType<typeof useChatBoxModelSelection>;
    readonly llmSettings: Readonly<Record<string, unknown>> | undefined;
    readonly onSetLLMSettings: ((settings: Readonly<Record<string, unknown>>) => void) | undefined;
    readonly onEditLlmSettings: () => void;
    readonly unattended: ChatBoxUnattended;
  };
  readonly extensions: {
    readonly contextIndicator: ReactNode;
    readonly editorCallbacks: ChatBoxEditorCallbacks | undefined;
    readonly entitySubmenus: PlusChatButtonEntitySubmenus | undefined;
  };
}

export function ChatBoxComposer(props: ChatBoxComposerProps): ReactNode {
  const { chatInputRef, refs, composer, state, data, conversation, participant, inputState, media, tools, extensions } = props;
  const { mentions, queued, steering } = composer;
  const { readAloud, voiceFeedback } = media;
  const { isStreaming, areAttachmentsDisabled, isInputLoading } = inputState;
  const { projectId, conversationId, isAgentsPage } = conversation;
  const { participantForEditor, entityActions } = participant;
  return (
    <>
      <ChatBoxVoicePlayer showPlayer={readAloud.showPlayer} voicePlayerProps={readAloud.voicePlayerProps} />
      <ChatBoxComposerQueue queue={queued} steering={steering} conversationKey={composer.queueKey} isStreaming={isStreaming} onInterrupt={composer.stopGeneration} input={chatInputRef} />
      <ChatBoxSteeringNotice notice={steering.notice} onDismiss={steering.dismissNotice} />
      <ChatBoxSteeringOffer offer={steering.offer} onSend={steering.acceptOffer} onDismiss={steering.dismissOffer} />
      <ChatBoxPopups
        {...buildChatBoxPopupsProps({
          state,
          onChangeParticipant: participant.onChangeParticipant,
          existingParticipants: conversation.conversationParticipants ?? [],
          projectId: conversation.projectIdString,
          onSelectUser: mentions.handleSelectUserMention,
          onSelectTool: mentions.handleSelectSkillTool,
          onAddParticipant: entityActions.onSelectParticipant,
          mentions,
        })}
      />
      {extensions.contextIndicator}
      <ChatBoxFloorTarget
        agents={mentions.addressableAgents}
        meta={conversation.conversationMeta}
        target={participant.target}
        participants={conversation.conversationParticipants}
        onSelect={participant.onChangeParticipant}
        onAddress={mentions.handleAddressAgent}
      />
      <NewChatInput
        ref={chatInputRef}
        conversationId={conversationId !== undefined ? String(conversationId) : undefined}
        state={{ isLoading: inputState.isComposerBusy, disabledInput: inputState.isDraftInputBusy, isStreaming, disabledSend: inputState.disabledSend, isCreatingConversation: data.lifecycle.isCreating, allowSendWhileStreaming: true }}
        content={{ placeholder: t('widgets.chatBox.inputPlaceholder', 'Type your message...'), clearInputAfterSubmit: true, slashHighlights: mentions.highlightRanges, draftKey: chatDraftKey({ projectId, conversationId, scratch: [isAgentsPage, conversation.editorTest] }) }}
        callbacks={{ onSend: steering.onSend, onStopGeneration: composer.stopTurn, onNormalKeyDown: state.onNormalKeyDown, onInputChange: mentions.onInputChange }}
        agentEditor={buildAgentEditorProps({
          participantForEditor,
          activeParticipantDetails: participant.agentEditorParticipantDetails,
          isAgentsPage,
          selectSavedOrDefaultModel: data.selectSavedOrDefaultModel,
          onShowParticipantsList: () => state.setShowRecommendationList(!state.showRecommendationList),
          onSelectVersion: (version) => {
            void participant.onSelectVersion(version);
          },
          editorCallbacks: extensions.editorCallbacks,
          onEditLlmSettings: tools.onEditLlmSettings,
        })}
        attachments={buildChatBoxAttachmentProps(data.attachments, areAttachmentsDisabled)}
        mentions={{ users: state.users, onMentionChange: mentions.handleMentionChange }}
        voice={{ isSpeakingMode: state.isSpeakingMode, onSpeakingModeToggle: () => state.setIsSpeakingMode(!state.isSpeakingMode), isTTSPlaying: readAloud.isPlaying, isRecording: voiceFeedback.isRecording, onError: voiceFeedback.onError }}
        slots={buildChatBoxInputSlots({
          attachments: { attachments: data.attachments.state.attachments, onAttachFiles: data.attachments.state.onAttachFiles, disabled: areAttachmentsDisabled },
          internalTools: { disabled: isInputLoading, tools: tools.internal.internalToolsButtonTools, onToolChange: tools.internal.handleInternalToolChange },
          model: { llmSettings: tools.llmSettings, onSetLLMSettings: tools.onSetLLMSettings, selectedModel: tools.model.selectedLlmModel, onSelectModel: tools.model.handleSelectModel, models: tools.model.modelsList, shown: !participantForEditor },
          clearChat: { disabled: shouldDisableClearChat(isStreaming, conversation.messageCount), onClear: composer.actions.handleClear },
          refs: { attachmentButtonRef: refs.attachmentButtonRef, voiceButtonRef: refs.voiceButtonRef, voiceInputRef: chatInputRef },
          voice: readAloud.voicePlayerProps,
          voiceInput: { onRecordingChange: voiceFeedback.onRecordingChange, onError: voiceFeedback.onError },
          isAgentsPage: !!isAgentsPage,
          participants: participant.normalisedParticipants,
          runToCompletion: { unattended: tools.unattended, disabled: isInputLoading },
          entitySubmenus: { ...extensions.entitySubmenus, onSelectParticipant: entityActions.onSelectParticipant, getParticipantMenuState: entityActions.getParticipantMenuState },
          ...buildCreateHandlerProps(extensions.editorCallbacks),
        })}
        refs={refs}
      />
    </>
  );
}
