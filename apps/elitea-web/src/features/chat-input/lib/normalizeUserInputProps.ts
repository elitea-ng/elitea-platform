import type { ReactNode } from 'react';

import type { Attachment } from '@/entities/attachment';

import type { MentionCandidate, MentionMatch } from './hooks/useMentionDetection.hooks';
import type {
  HighlightRange,
  UserInputAttachmentsProps,
  UserInputCallbacks,
  UserInputSendButtonConfig,
  UserInputSlotProps,
  UserInputSlots,
  UserInputVoiceProps,
} from '../ui/UserInput.types';

/**
 * Pure prop-group normalisers for `UserInput.tsx` — each resolves one
 * optional prop-bag into a fully-defaulted, flat shape. Split into small,
 * single-purpose functions (rather than one big normaliser) purely to keep
 * every function under the §3.5 cyclomatic-complexity budget (≤12) — each
 * one below sits at 2-7. Not hooks (no React hook calls) — plain functions,
 * so they live in `lib/`, not `lib/hooks/`.
 */

export function resolveField(field: UserInputSlotProps['field']): {
  readonly placeholder: string | undefined;
  readonly color: string | undefined;
  readonly iconColor: string | undefined;
} {
  return { placeholder: field?.placeholder, color: field?.color, iconColor: field?.iconColor };
}

export function resolveMention(mention: UserInputSlotProps['mention']): {
  readonly users: readonly MentionCandidate[];
  readonly onMentionChange: ((mentions: readonly MentionMatch[]) => void) | undefined;
} {
  return { users: mention?.users ?? [], onMentionChange: mention?.onMentionChange };
}

export function resolveTooltip(tooltip: UserInputSlotProps['tooltip']): {
  readonly title: string;
  readonly placement: NonNullable<UserInputSlotProps['tooltip']>['placement'];
} {
  return { title: tooltip?.title ?? '', placement: tooltip?.placement ?? 'top' };
}

export function resolveHighlightRanges(highlight: UserInputSlotProps['highlight']): readonly HighlightRange[] {
  return highlight?.ranges ?? [];
}

/** `sendButton` config is passed straight through to the injected `sendControl` slot (styling is the composition root's concern) — only `tooltipOfSendButton` needs a safe default here. */
export function resolveSendButtonConfig(sendButton: UserInputSlotProps['sendButton']): UserInputSendButtonConfig & {
  readonly tooltipOfSendButton: string | undefined;
} {
  return { ...sendButton, tooltipOfSendButton: sendButton?.tooltipOfSendButton };
}

/**
 * Which of the two composer end-controls the footer shows.
 *
 * A17: the send control is no longer the "else" of Stop. A host that QUEUES
 * what is sent during a run (`UserInputSendButtonConfig.keepWhileStreaming`)
 * shows both; every other host keeps the baseline's either/or. A pure function
 * because `UserInput` itself sits on the §3.5 complexity ceiling.
 */
export function resolveFooterControls(flags: {
  readonly isStreaming: boolean;
  readonly isUploading: boolean;
  readonly keepSendWhileStreaming: boolean | undefined;
}): { readonly showStop: boolean; readonly showSend: boolean } {
  const showStop = flags.isStreaming && !flags.isUploading;
  return { showStop, showSend: !showStop || flags.keepSendWhileStreaming === true };
}

export function resolveAttachmentsProps(attachments: UserInputAttachmentsProps | undefined): {
  readonly items: readonly Attachment[];
  readonly onDelete: UserInputAttachmentsProps['onDelete'];
  readonly isUploading: boolean;
  readonly uploadProgress: number;
} {
  return {
    items: attachments?.items ?? [],
    onDelete: attachments?.onDelete,
    isUploading: attachments?.isUploading ?? false,
    uploadProgress: attachments?.uploadProgress ?? 0,
  };
}

export function resolveVoiceProps(voice: UserInputVoiceProps | undefined): {
  readonly isSpeakingMode: boolean;
  readonly isRecording: boolean;
  readonly onEnterSpeakingMode: UserInputVoiceProps['onEnterSpeakingMode'];
  readonly onExitSpeakingMode: UserInputVoiceProps['onExitSpeakingMode'];
} {
  return {
    isSpeakingMode: voice?.isSpeakingMode ?? false,
    isRecording: voice?.isRecording ?? false,
    onEnterSpeakingMode: voice?.onEnterSpeakingMode,
    onExitSpeakingMode: voice?.onExitSpeakingMode,
  };
}

export function resolveCallbacks(callbacks: UserInputCallbacks | undefined): UserInputCallbacks {
  return {
    onSend: callbacks?.onSend,
    onStop: callbacks?.onStop,
    onNormalKeyDown: callbacks?.onNormalKeyDown,
    onInputChange: callbacks?.onInputChange,
    onFilePaste: callbacks?.onFilePaste,
  };
}

/** `slots.attachmentList` is only ever invoked once there is something to show it. */
export function renderAttachmentList(
  items: readonly Attachment[],
  attachmentList: UserInputSlots['attachmentList'],
  onDeleteAttachment: ((index: number) => void) | undefined,
  disabled: boolean,
): ReactNode {
  if (items.length === 0) return null;
  return attachmentList?.({ attachments: items, onDeleteAttachment, disabled }) ?? null;
}
