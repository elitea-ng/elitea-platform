import type { DragEvent, KeyboardEvent, ReactNode } from 'react';
import type { TooltipProps } from '@mui/material/Tooltip';

import type { Attachment } from '@/entities/attachment';

import type { ChatInputHandle } from '../lib/chatInputHandle';
import type { MentionCandidate, MentionMatch } from '../lib/hooks/useMentionDetection.hooks';

/**
 * Type/constant module for `UserInput.tsx`, split out purely to keep that
 * file (and its per-function cyclomatic complexity) under the §3.5 budgets
 * (≤400 lines/file, ≤12 per component) — the same "split the file, not the
 * component's contract" reasoning `features/toolkits/ui/ToolkitEditor.tsx`
 * / `ToolkitEditorParts.tsx` already established for this batch.
 */

export const MAX_ROWS = 10;
export const MIN_ROWS = 2;
export const MIN_HEIGHT = 70;

/** A `[start, end)` character range highlighted in the input's mirrored overlay text (slash-command / mention matches). */
export interface HighlightRange {
  readonly start: number;
  readonly end: number;
}

/** @public The imperative handle `NewChatInput.tsx` proxies and `lib/chatInputHandle.ts`'s `ChatInputHandle` consumers narrow to. */
export interface UserInputHandle extends ChatInputHandle {
  focus(): void;
  reset(): void;
  getInputContent(): string;
  getCursorPosition(): number | null;
  /**
   * The caret position as of the last time the user actually EDITED the text
   * (issue #933, ELITEA-1317), or `null` when they have not edited it in this
   * composer instance. Distinct from `getCursorPosition()`, which reports
   * wherever the caret happens to sit — including a position reached purely by
   * a mouse click. Voice dictation inserts at the last EDITED position, so a
   * bare click before reaching for the mic does not drop the transcript into
   * the middle of a sentence the user had finished typing.
   */
  getLastEditPosition(): number | null;
  setValue(value: string, cursorPosition?: number): void;
  replaceRange(start: number, end: number, text: string): void;
  /**
   * **Baseline parity, disclosed, NOT fixed**: when `symbol` is not found in
   * the current content, `lastIndexOf` returns `-1` and `slice(0, -1)`
   * truncates the LAST character of the whole input — unrelated to
   * `symbol` at all. Reproduced byte-for-byte from `UserInput.jsx`'s own
   * `removeSymbol`, rather than guarding it away, because at least one
   * caller (`useSlashMention`/`useChatSkillMention`-style hooks, a sibling
   * cluster in this same unit) may already depend on this exact fallback
   * shape for its own cancel/backspace handling.
   */
  removeSymbol(symbol: string): void;
  sendQuestion(): void;
  insertTextAtCursor(textToInsert: string): void;
  mentionUser(userString: string): void;
}

export interface UserInputSendButtonConfig {
  readonly iconColor?: string | undefined;
  readonly disabledBackground?: string | undefined;
  readonly background?: string | undefined;
  readonly size?: string | undefined;
  /** Baseline: `NewChatInput.jsx`'s own `tooltipOfSendButton` prop, threaded through `UserInput`'s `tooltipOfSendButton` prop into `SendButton`. Folded into this existing config bag (rather than spending a new top-level `UserInput`/`NewChatInput` prop slot) since it travels the exact same path as the rest of this object. */
  readonly tooltipOfSendButton?: string | undefined;
  /**
   * A17 (ELITEA-2871): keep the send control on screen WHILE a turn is open,
   * beside Stop rather than replaced by it — the host queues what is sent
   * there ("Waiting messages") instead of putting a second turn on the wire.
   *
   * Off by default, so every host that has no queue keeps the baseline's
   * either/or footer. Folded into this config bag for the reason
   * `tooltipOfSendButton` above was: `UserInputProps` sits exactly on the §3.5
   * 12-prop budget, and this travels the same path as the rest of the object.
   */
  readonly keepWhileStreaming?: boolean | undefined;
}

/** Everything the real `SendButton`/stop-button (baseline: `features/chat/ui/chat-button/SendButton.jsx`, unit C6) consumed — read from that file directly to build this bag. */
export interface UserInputSendControlSlotProps {
  readonly isSpeakingMode: boolean;
  /**
   * A voice recording (dictation OR the speaking-mode loop) is capturing right
   * now (#932, ELITEA-1295). Separate from `disabledSend` on purpose: it must
   * block the control that would put the composer into a SECOND voice mode —
   * the speaking-mode wave icon — without blocking Send itself, which is still
   * a legitimate way to commit dictated text, and without blocking the
   * imperative `sendQuestion()` the speaking-mode loop's own auto-send fires
   * while recording (#931).
   */
  readonly isRecording: boolean;
  readonly question: string;
  readonly disabledSend: boolean;
  readonly onEnterSpeakingMode: (() => void) | undefined;
  readonly onExitSpeakingMode: (() => void) | undefined;
  readonly onSend: () => void;
  readonly tooltipOfSendButton: string | undefined;
  readonly config: UserInputSendButtonConfig | undefined;
}

/** Everything the real `HighlightedText` (baseline: `features/chat/ui/highlighted-text/HighlightedText.jsx`, unit C4) consumed. */
export interface UserInputHighlightOverlaySlotProps {
  readonly text: string;
  readonly ranges: readonly HighlightRange[];
}

/** Everything the real `FileList` (baseline: `components/Chat/FileList.jsx`, unit C4's `ownedPaths`) consumed. */
export interface UserInputAttachmentListSlotProps {
  readonly attachments: readonly Attachment[];
  readonly onDeleteAttachment: ((index: number) => void) | undefined;
  readonly disabled: boolean;
}

export interface UserInputSlots {
  /** Pre-existing baseline extension point (caller content in the footer row) — unrelated to the three slots below. */
  readonly footer?: ReactNode | undefined;
  readonly sendControl?: ((props: UserInputSendControlSlotProps) => ReactNode) | undefined;
  readonly highlightOverlay?: ((props: UserInputHighlightOverlaySlotProps) => ReactNode) | undefined;
  readonly attachmentList?: ((props: UserInputAttachmentListSlotProps) => ReactNode) | undefined;
}

export interface UserInputSlotProps {
  readonly container?: { readonly onDrop?: ((event: DragEvent<HTMLDivElement>) => void) | undefined } | undefined;
  readonly field?:
    | { readonly placeholder?: string | undefined; readonly color?: string | undefined; readonly iconColor?: string | undefined }
    | undefined;
  readonly mention?:
    | {
        readonly users?: readonly MentionCandidate[] | undefined;
        readonly onMentionChange?: ((mentions: readonly MentionMatch[]) => void) | undefined;
      }
    | undefined;
  readonly tooltip?: { readonly title?: string | undefined; readonly placement?: TooltipProps['placement'] | undefined } | undefined;
  readonly highlight?: { readonly ranges?: readonly HighlightRange[] | undefined } | undefined;
  readonly sendButton?: UserInputSendButtonConfig | undefined;
  readonly stopButton?: { readonly iconColor?: string | undefined; readonly tooltipTitle?: string | undefined } | undefined;
}

export interface UserInputCallbacks {
  readonly onSend?: ((question: string, inputContent: string) => void) | undefined;
  readonly onStop?: (() => void) | undefined;
  /**
   * Typed against `HTMLDivElement`, not `HTMLTextAreaElement` — MUI's
   * `TextField` types every native DOM event handler prop (`onKeyDown`/
   * `onPaste`/composition events) against its `FormControl` root element
   * regardless of `multiline`, matching the established convention already
   * used for the same situation elsewhere in this codebase (e.g.
   * `features/pipelines/ui/AIPromptInput.tsx`'s own multiline `TextField`
   * `onKeyDown`).
   */
  readonly onNormalKeyDown?: ((event: KeyboardEvent<HTMLDivElement>) => void) | undefined;
  readonly onInputChange?: ((value: string) => void) | undefined;
  readonly onFilePaste?: ((files: File | readonly File[]) => void) | undefined;
}

export interface UserInputAttachmentsProps {
  readonly items?: readonly Attachment[] | undefined;
  readonly onDelete?: ((index: number) => void) | undefined;
  readonly isUploading?: boolean | undefined;
  readonly uploadProgress?: number | undefined;
}

export interface UserInputVoiceProps {
  readonly isSpeakingMode?: boolean | undefined;
  readonly isRecording?: boolean | undefined;
  readonly onEnterSpeakingMode?: (() => void) | undefined;
  readonly onExitSpeakingMode?: (() => void) | undefined;
}

/** @public shared/features component API. §3.5 budget: exactly 12 top-level props (grouped). */
export interface UserInputProps {
  readonly dataTourTargetId?: string | undefined;
  readonly slots: UserInputSlots;
  readonly slotProps?: UserInputSlotProps | undefined;
  readonly callbacks?: UserInputCallbacks | undefined;
  readonly attachments?: UserInputAttachmentsProps | undefined;
  readonly voice?: UserInputVoiceProps | undefined;
  readonly clearInputAfterSend?: boolean | undefined;
  readonly disabledSend?: boolean | undefined;
  readonly disabledInput?: boolean | undefined;
  readonly showLoading?: boolean | undefined;
  readonly isStreaming?: boolean | undefined;
  readonly isCreatingConversation?: boolean | undefined;
}
