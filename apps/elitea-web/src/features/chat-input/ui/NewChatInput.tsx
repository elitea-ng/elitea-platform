import type { ReactNode, Ref } from 'react';
import { forwardRef } from 'react';

import { t } from '@/shared/i18n';

import { useSpeakingModeLoop } from '../lib/hooks/useSpeakingModeLoop';
import {
  useNewChatInputAttachmentBridge,
  useNewChatInputImperativeHandle,
  useNewChatInputInputChange,
  useNewChatInputRefs,
  useNewChatInputSend,
  useStopVoiceOnConversationChange,
} from '../lib/hooks/useNewChatInputController.hooks';
import {
  resolveAttachments,
  resolveCallbacks,
  resolveContent,
  resolveMentions,
  resolveRefs,
  resolveSlots,
  resolveState,
  resolveVoice,
} from '../lib/normalizeNewChatInputProps';

import type { NewChatInputHandle, NewChatInputProps } from './NewChatInput.types';
import { NewChatInputFooterContent } from './NewChatInputFooterContent';
import { UserInput } from './UserInput';

// Only `NewChatInputHandle`/`NewChatInputProps` are re-exported from this
// path (knip: `NewChatInput.test.tsx` is the one real consumer that imports
// via `'./NewChatInput'`, matching this component's default export site).
// `NewChatInputAttachmentsProps`, `NewChatInputCallbacks`,
// `NewChatInputContentProps`, `NewChatInputMentionProps`, `NewChatInputRefs`,
// `NewChatInputSlots`, `NewChatInputStateProps`, `NewChatInputVoiceProps`,
// `AttachmentButtonHandle`, `VoiceButtonHandle` each have real consumers too
// (`../lib/normalizeNewChatInputProps.ts`, `../lib/hooks/
// useNewChatInputController.hooks.ts`, `./NewChatInputFooterContent.tsx`)
// but every one of them imports directly from `'./NewChatInput.types'`, not
// through this re-export — so re-exporting them here too would just be a
// second, unused path to the same symbol. `NewChatInputAgentEditorProps` has
// no consumer anywhere outside `NewChatInput.types.ts` itself, so that one
// isn't even `export`ed from its defining module anymore (see that file).
export type { NewChatInputHandle, NewChatInputProps } from './NewChatInput.types';

/**
 * Port of `apps/elitea-ui/src/pages/NewChat/NewChatInput.jsx` (unit C3,
 * "chat-input" cluster — this unit's composition root). Split across
 * `NewChatInput.types.ts` (props/slots/handle types),
 * `NewChatInputFooterContent.tsx` (the footer button row, including the
 * `AgentEditorPanel`-vs-`modelSelector` branch this cluster still owns),
 * `../lib/normalizeNewChatInputProps.ts` (prop-group defaulting) and
 * `../lib/hooks/useNewChatInputController.hooks.ts` (imperative-handle
 * proxy, voice/attachment bridging) — purely to keep this file, and every
 * function in it, under the §3.5 budgets.
 *
 * **Voice orchestration**: `useSpeakingModeLoop` (sibling cluster, this
 * same unit) — `userInputRef` (this component's own `UserInput` ref)
 * satisfies its `SpeakingModeInputHandle` contract structurally.
 *
 * **Required architecture deviation, same class as `UserInput.tsx`'s own
 * (this unit's task brief), MORE so here**: the baseline's footer composes
 * `PlusChatButton`/`ChatButton.AttachmentButton`, `ChatButton
 * .ChatInternalToolsConfigButton`, `ChatButton.VoiceButton` (all unit C6),
 * `AgentEditorPanel` (owned by THIS cluster — the real, internally-
 * rendered branch, see `NewChatInputFooterContent.tsx`), and
 * `LLMModelSelector` (`widgets/llm-model-selector` — confirmed NOT owned
 * by any tracked Wave-2 unit, a genuine ungoverned gap). Every C6/
 * ungoverned piece is an injected `slots.*` `ReactNode` — see
 * `NewChatInput.types.ts`'s module doc for the full slot list.
 *
 * **`attachmentButtonRef`/`voiceButtonRef` — disclosed design choice**: the
 * baseline orchestrates two things THROUGH refs to the concrete
 * `AttachmentButton`/`VoiceButton` it renders directly: (1) delegating
 * drop/paste validation to `attachmentButtonRef.current.onDrop(...)`, and
 * (2) stopping the mic via `voiceButtonRef.current.stop()` on send and on
 * conversation change. Since those components are now `slots.*`-injected
 * (built entirely by a future composition-root unit), this component
 * cannot create+attach those refs itself — instead it accepts them as
 * `refs.attachmentButtonRef`/`refs.voiceButtonRef` (paired with the
 * `ReactNode` slot, not a replacement for it): the composition root
 * creates the ref, passes it here, AND attaches it to the real component
 * it puts in the slot. This preserves the baseline's exact orchestration
 * logic (still owned by THIS component) while respecting the layer
 * boundary — an explicit, disclosed judgment call, not a silent drop.
 * Without a ref supplied, drop/paste and the mic-stop calls are all
 * no-ops — matching baseline exactly (`attachmentButtonRef.current` guards
 * both `onDrop`/`handleFilePaste` there too; there is no unvalidated
 * fallback path in the original).
 *
 * **`VoiceButton`'s `inputRef` — open gap, as of this writing**: baseline
 * wires the SAME ref object to both `<UserInput ref={userInputRef}>` and
 * `<ChatButton.VoiceButton inputRef={userInputRef}>`, so recognized speech
 * writes directly into the composer. `userInputRef` (`useNewChatInputRefs()`
 * below) is created and consumed entirely inside this component, but the
 * imperative handle THIS component itself exposes on its own forwarded
 * `ref` (`useNewChatInputImperativeHandle`, `NewChatInputHandle`) is already
 * structurally identical to what `VoiceButton`'s `inputRef` needs
 * (`getInputContent`/`getCursorPosition`/`setValue`/`focus`) — so the
 * composition root does not need a new slot or prop here at all. It only
 * needs to pass the SAME ref it already holds from `<NewChatInput ref=
 * {chatInputRef}>` (`widgets/chat-box/ui/ChatBox.tsx`) into `slots
 * .voiceButton`'s `<VoiceButton inputRef={chatInputRef}>`. As of this
 * writing `widgets/chat-box/ui/ChatBoxInputSlots.tsx` never receives
 * `chatInputRef`, so it builds `<VoiceButton inputRef={undefined}>` —
 * dictation transcribes but never writes into the input at all.
 *
 * **`isRecording` — disclosed judgment call**: the baseline owned a local
 * `isRecording` `useState`, flipped by `VoiceButton`'s own `onRecordingChange`
 * callback (which ALSO auto-exited speaking mode:
 * `if (recording && isSpeakingMode) onSpeakingModeToggle?.()`). Since
 * `VoiceButton` is now `slots.voiceButton`-injected, this component no
 * longer owns a callback wired to its internals — `voice.isRecording` is a
 * plain CONTROLLED prop instead; the composition root that builds the real
 * `VoiceButton` is responsible for feeding it back (and for the
 * recording-vs-speaking-mode coupling the baseline's callback also did).
 *
 * **`data-tour` — dropped**: `features/interactive-tours` does not exist
 * in this worktree, same disclosed gap `features/chat-conversation-list/ui
 * /conversations/Conversations.tsx`'s own doc comment already established
 * for the identical dependency.
 *
 * **`useNewInputKeyDownHandler`/`useNewStartConversationInputKeyDownHandler`/
 * `useAttachmentToolChange`** are NOT called from this component — the
 * baseline itself does not call them from `NewChatInput.jsx` either (see
 * `../lib/hooks/useInputKeyDownHandler.hooks.ts`/`useAttachmentToolChange
 * .hooks.ts`'s own doc comments); they are exported from this slice's
 * public barrel for a future composition-root consumer.
 */
export const NewChatInput = forwardRef(function NewChatInput(
  props: NewChatInputProps,
  ref: Ref<NewChatInputHandle>,
): ReactNode {
  const { conversationId, agentEditor } = props;
  const state = resolveState(props.state);
  const content = resolveContent(props.content);
  const callbacks = resolveCallbacks(props.callbacks);
  const attachments = resolveAttachments(props.attachments);
  const mentions = resolveMentions(props.mentions);
  const voice = resolveVoice(props.voice);
  const slots = resolveSlots(props.slots);
  const refs = resolveRefs(props.refs);

  const { userInputRef } = useNewChatInputRefs();

  const { isRecording: isSpeakingModeRecording, pauseForRegeneration, notifyManualEdit } = useSpeakingModeLoop({
    isSpeakingMode: voice.isSpeakingMode,
    inputRef: userInputRef,
    isStreaming: state.isStreaming,
    isTTSPlaying: voice.isTTSPlaying,
  });

  useNewChatInputImperativeHandle({ ref, userInputRef, pauseForRegeneration });
  useStopVoiceOnConversationChange(refs.voiceButtonRef, conversationId);

  const handleSend = useNewChatInputSend({ voiceButtonRef: refs.voiceButtonRef, onSend: callbacks.onSend });
  const handleInputChange = useNewChatInputInputChange(callbacks.onInputChange, notifyManualEdit);
  const { onDrop, onFilePaste } = useNewChatInputAttachmentBridge({
    attachmentButtonRef: refs.attachmentButtonRef,
    disabled: attachments.disabled,
  });

  const finalIsRecording = voice.isRecording || isSpeakingModeRecording;
  // Baseline `slotProps.input.placeholder`: swap to the "speak your message"
  // hint while either kind of voice recording is active.
  const placeholder =
    finalIsRecording || voice.isSpeakingMode
      ? t('chatInput.newChatInput.speakPlaceholder', 'Speak your message')
      : content.placeholder;

  return (
    <UserInput
      ref={userInputRef}
      slots={{
        footer: (
          <NewChatInputFooterContent
            slots={slots}
            isAgentsPage={agentEditor.isAgentsPage ?? false}
            activeParticipant={agentEditor.activeParticipant}
            agentEditorProps={{
              activeParticipant: agentEditor.activeParticipant,
              participantDetails: agentEditor.activeParticipantDetails,
              disabled: state.isStreaming,
              disableSwitchToModel: agentEditor.disableSwitchingParticipant || state.isLoading || state.isStreaming,
              isEditorDirty: state.isEditorDirty,
              onClickParticipant: agentEditor.onShowParticipantsList,
              onSwitchToModel: agentEditor.selectSavedOrDefaultModel,
              version: {
                selectedVersionId: agentEditor.selectedVersionId,
                onSelect: agentEditor.onSelectVersion,
                onShowVersionChangeAlert: agentEditor.onShowVersionChangeAlert,
                onRefresh: agentEditor.onRefreshParticipantDetails,
              },
              variablesEditor: { variables: agentEditor.variables, onChange: agentEditor.onChangeVariables },
              editorNav: {
                onShowAgentEditor: agentEditor.onShowAgentEditor,
                onShowPipelineEditor: agentEditor.onShowPipelineEditor,
                onCloseAgentEditor: agentEditor.onCloseAgentEditor,
                onClosePipelineEditor: agentEditor.onClosePipelineEditor,
              },
            }}
          />
        ),
        sendControl: slots.sendControl,
        highlightOverlay: slots.highlightOverlay,
        attachmentList: slots.attachmentList,
      }}
      slotProps={{
        container: { onDrop },
        field: { placeholder },
        mention: mentions,
        highlight: { ranges: content.slashHighlights },
        sendButton: { tooltipOfSendButton: content.tooltipOfSendButton },
      }}
      callbacks={{
        onSend: handleSend,
        onStop: callbacks.onStopGeneration,
        onNormalKeyDown: callbacks.onNormalKeyDown,
        onInputChange: handleInputChange,
        onFilePaste,
      }}
      attachments={{
        items: attachments.items,
        onDelete: attachments.onDeleteAttachment,
        isUploading: attachments.isUploading,
        uploadProgress: attachments.uploadProgress,
      }}
      voice={{
        isSpeakingMode: voice.isSpeakingMode,
        isRecording: finalIsRecording,
        onEnterSpeakingMode: voice.onSpeakingModeToggle,
        onExitSpeakingMode: voice.onSpeakingModeToggle,
      }}
      clearInputAfterSend={content.clearInputAfterSubmit}
      disabledSend={state.disabledSend || finalIsRecording}
      disabledInput={state.isLoading}
      showLoading={state.isLoading}
      isStreaming={state.isStreaming}
      isCreatingConversation={state.isCreatingConversation}
    />
  );
});
