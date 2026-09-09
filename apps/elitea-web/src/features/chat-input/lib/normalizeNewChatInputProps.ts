import type {
  NewChatInputAttachmentsProps,
  NewChatInputCallbacks,
  NewChatInputContentProps,
  NewChatInputMentionProps,
  NewChatInputRefs,
  NewChatInputSlots,
  NewChatInputStateProps,
  NewChatInputVoiceProps,
} from '../ui/NewChatInput.types';

/**
 * Pure prop-group normalisers for `NewChatInput.tsx` — same "small,
 * single-purpose functions instead of one big normaliser" reasoning as
 * `normalizeUserInputProps.ts`.
 */

export function resolveState(state: NewChatInputStateProps | undefined): {
  readonly isLoading: boolean;
  readonly isStreaming: boolean;
  readonly disabledSend: boolean;
  readonly isCreatingConversation: boolean;
  readonly isEditorDirty: boolean;
} {
  return {
    isLoading: state?.isLoading ?? false,
    isStreaming: state?.isStreaming ?? false,
    disabledSend: state?.disabledSend ?? false,
    isCreatingConversation: state?.isCreatingConversation ?? false,
    isEditorDirty: state?.isEditorDirty ?? false,
  };
}

export function resolveContent(content: NewChatInputContentProps | undefined): {
  readonly placeholder: string;
  readonly clearInputAfterSubmit: boolean;
  readonly tooltipOfSendButton: string | undefined;
  readonly slashHighlights: readonly { readonly start: number; readonly end: number }[];
} {
  return {
    placeholder: content?.placeholder ?? '',
    clearInputAfterSubmit: content?.clearInputAfterSubmit ?? true,
    tooltipOfSendButton: content?.tooltipOfSendButton,
    slashHighlights: content?.slashHighlights ?? [],
  };
}

export function resolveCallbacks(callbacks: NewChatInputCallbacks | undefined): NewChatInputCallbacks {
  return {
    onSend: callbacks?.onSend,
    onStopGeneration: callbacks?.onStopGeneration,
    onNormalKeyDown: callbacks?.onNormalKeyDown,
    onInputChange: callbacks?.onInputChange,
  };
}

export function resolveAttachments(attachments: NewChatInputAttachmentsProps | undefined): {
  readonly items: readonly File[];
  readonly onAttachFiles: NewChatInputAttachmentsProps['onAttachFiles'];
  readonly onDeleteAttachment: NewChatInputAttachmentsProps['onDeleteAttachment'];
  readonly disabled: boolean;
  readonly isUploading: boolean;
  readonly uploadProgress: number;
} {
  return {
    items: attachments?.items ?? [],
    onAttachFiles: attachments?.onAttachFiles,
    onDeleteAttachment: attachments?.onDeleteAttachment,
    disabled: attachments?.disabled ?? false,
    isUploading: attachments?.isUploading ?? false,
    uploadProgress: attachments?.uploadProgress ?? 0,
  };
}

export function resolveMentions(mentions: NewChatInputMentionProps | undefined): {
  readonly users: NonNullable<NewChatInputMentionProps['users']>;
  readonly onMentionChange: NewChatInputMentionProps['onMentionChange'];
} {
  return { users: mentions?.users ?? [], onMentionChange: mentions?.onMentionChange };
}

export function resolveVoice(voice: NewChatInputVoiceProps | undefined): {
  readonly isSpeakingMode: boolean;
  readonly onSpeakingModeToggle: NewChatInputVoiceProps['onSpeakingModeToggle'];
  readonly isTTSPlaying: boolean;
  readonly isRecording: boolean;
} {
  return {
    isSpeakingMode: voice?.isSpeakingMode ?? false,
    onSpeakingModeToggle: voice?.onSpeakingModeToggle,
    isTTSPlaying: voice?.isTTSPlaying ?? false,
    isRecording: voice?.isRecording ?? false,
  };
}

export function resolveSlots(slots: NewChatInputSlots | undefined): NewChatInputSlots {
  return {
    sendControl: slots?.sendControl,
    highlightOverlay: slots?.highlightOverlay,
    attachmentList: slots?.attachmentList,
    attachmentButton: slots?.attachmentButton,
    internalToolsConfig: slots?.internalToolsConfig,
    // Every slot must be listed here. This function rebuilds the bundle
    // field by field, so a slot the composition root supplies and this list
    // omits is silently dropped between the two — the component renders, the
    // builder is called, and the control simply never appears.
    clearChat: slots?.clearChat,
    voiceButton: slots?.voiceButton,
    modelSelector: slots?.modelSelector,
  };
}

export function resolveRefs(refs: NewChatInputRefs | undefined): NewChatInputRefs {
  return { attachmentButtonRef: refs?.attachmentButtonRef, voiceButtonRef: refs?.voiceButtonRef };
}
