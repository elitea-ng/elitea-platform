import type { KeyboardEvent, ReactNode, RefObject } from 'react';

import type { Participant } from '@/entities/participant';
import type { VersionSummary } from '@/entities/version';

import type { MentionCandidate, MentionMatch } from '../lib/hooks/useMentionDetection.hooks';
import type {
  UserInputAttachmentListSlotProps,
  UserInputHandle,
  UserInputHighlightOverlaySlotProps,
  UserInputSendControlSlotProps,
} from './UserInput.types';
import type { AgentEditorParticipantDetails } from './AgentEditorPanel.types';
import type { AgentVariable } from './VariablesEditor.types';

/**
 * Type module for `NewChatInput.tsx`, split out purely to keep that file
 * (and its per-function cyclomatic complexity) under the §3.5 budgets —
 * same rationale as `UserInput.types.ts`/`AgentEditorPanel.types.ts`.
 */

/** @public The imperative handle proxied from `UserInput`'s own, plus the speaking-mode pause this wrapper adds. */
export interface NewChatInputHandle extends UserInputHandle {
  pauseSpeakingMode(): void;
}

/**
 * The real `SendButton`/stop-button and `HighlightedText` (units C6/C4)
 * cannot be imported here either (`no-sideways-features`) — these two
 * slots are pure pass-throughs down to `UserInput`'s own identically-named
 * slots, unchanged, so the composition root that renders this component
 * (`widgets/chat-box`, unit C6) supplies them once for both layers.
 *
 * CLOSED (this comment used to record it as open): `widgets/chat-box/ui
 * /ChatBoxInputSlots.tsx`'s `buildChatBoxInputSlots()` now sets `sendControl`,
 * `highlightOverlay` AND `attachmentList`. Do NOT close a future slot gap by
 * importing the component here — that would reintroduce the exact
 * `no-sideways-features` violation these slots exist to avoid; fill it from
 * the composition root instead.
 */
export interface NewChatInputSlots {
  readonly sendControl?: ((props: UserInputSendControlSlotProps) => ReactNode) | undefined;
  readonly highlightOverlay?: ((props: UserInputHighlightOverlaySlotProps) => ReactNode) | undefined;
  /**
   * The chips for the files staged on the NEXT message — baseline renders
   * `FileList` directly inside `UserInput` (`ComponentsLib/Chat/UserInput
   * .jsx:422-427`). `FileList` lives in `features/chat-messages`, so the same
   * `no-sideways-features` rule that makes `sendControl` a slot makes this one
   * a slot too, and the composition root supplies it once for both layers.
   *
   * Without it the composer counts a picked file (the "+" menu's "N left"
   * ticks down) and sends it, but shows the user NOTHING: no chip, no name, no
   * way to remove it before sending. That is what the slot existing and nobody
   * filling it looked like from the outside.
   */
  readonly attachmentList?: ((props: UserInputAttachmentListSlotProps) => ReactNode) | undefined;
  /**
   * Baseline: `PlusChatButton` (fromTheChat) / `ChatButton.AttachmentButton`
   * (unit C6, genuine `no-sideways-features` cross-unit import either way).
   * The composition root renders whichever flavour it needs and wires
   * `refs.attachmentButtonRef` (see `NewChatInputRefs`) to it.
   */
  readonly attachmentButton?: ReactNode | undefined;
  /** Baseline: `ChatButton.ChatInternalToolsConfigButton` (unit C6). */
  readonly internalToolsConfig?: ReactNode | undefined;
  /**
   * The conversation's own "clear the history" control
   * (`ChatButton.ClearChatButton`).
   *
   * A slot rather than a component this cluster builds, for the reason every
   * other control here is one: the action it fires belongs to whatever owns
   * the transcript. The composition root withholds it (passes `undefined`)
   * on the agent/pipeline editor surface, which mounts its own clear control
   * beside the panel and would otherwise show two.
   */
  readonly clearChat?: ReactNode | undefined;
  /** Baseline: `ChatButton.VoiceButton` (unit C6) — see `refs.voiceButtonRef`. */
  readonly voiceButton?: ReactNode | undefined;
  /** Baseline: `widgets/llm-model-selector`'s `LLMModelSelector` — confirmed NOT owned by any tracked Wave-2 unit (a genuine ungoverned gap). Rendered only as the fallback branch — see this file's module doc for the exact baseline branch condition preserved. */
  readonly modelSelector?: ReactNode | undefined;
}

/**
 * Injected imperative refs pairing each `ReactNode` slot above with a
 * handle the composition root attaches to whatever concrete component it
 * renders into that slot — lets `NewChatInput` keep the baseline's own
 * orchestration (stop the mic on send/conversation change; delegate
 * drop/paste validation to the real attachment button) without importing
 * either concrete component. See this file's module doc for the full
 * disclosure of this design choice.
 *
 * **Open gap, as of this writing**: `widgets/chat-box/ui/ChatBox.tsx`
 * passes `refs={{}}` — it creates neither ref nor attaches either one to
 * the `AttachmentButton`/`VoiceButton` it builds in `ChatBoxInputSlots
 * .tsx`. Both target components are already `forwardRef`-based and export
 * `AttachmentButtonHandle`/`VoiceButtonHandle` (structurally identical to
 * this file's own copies below) for exactly this purpose — closing this
 * gap is composition-root wiring only, not a type or contract change.
 * Without it, drop/paste-to-attach and mic-auto-stop-on-send/conversation-
 * change are silent no-ops (matches this hook's own guarded fallback, see
 * `useNewChatInputAttachmentBridge`/`useStopVoiceOnConversationChange` in
 * `../lib/hooks/useNewChatInputController.hooks.ts`).
 */
export interface NewChatInputRefs {
  readonly attachmentButtonRef?: RefObject<AttachmentButtonHandle | null> | undefined;
  readonly voiceButtonRef?: RefObject<VoiceButtonHandle | null> | undefined;
}

export interface AttachmentButtonHandle {
  onDrop(event: { readonly dataTransfer: { readonly files: readonly File[] }; preventDefault(): void }): void;
}

export interface VoiceButtonHandle {
  stop(): void;
}

export interface NewChatInputStateProps {
  readonly isLoading?: boolean | undefined;
  readonly isStreaming?: boolean | undefined;
  readonly disabledSend?: boolean | undefined;
  readonly isCreatingConversation?: boolean | undefined;
  readonly isEditorDirty?: boolean | undefined;
}

export interface NewChatInputContentProps {
  readonly placeholder?: string | undefined;
  readonly clearInputAfterSubmit?: boolean | undefined;
  readonly tooltipOfSendButton?: string | undefined;
  readonly slashHighlights?: readonly { readonly start: number; readonly end: number }[] | undefined;
}

export interface NewChatInputCallbacks {
  readonly onSend?: ((question: string, inputContent: string) => void) | undefined;
  readonly onStopGeneration?: (() => void) | undefined;
  readonly onNormalKeyDown?: ((event: KeyboardEvent<HTMLDivElement>) => void) | undefined;
  readonly onInputChange?: ((value: string) => void) | undefined;
}

// Not exported (knip: no outside consumer by name, unlike this file's other
// prop-group types below) — only referenced as `NewChatInputProps.agentEditor`'s
// field type.
interface NewChatInputAgentEditorProps {
  readonly activeParticipant: Participant | undefined;
  readonly activeParticipantDetails: AgentEditorParticipantDetails | undefined;
  readonly isAgentsPage?: boolean | undefined;
  readonly disableSwitchingParticipant?: boolean | undefined;
  readonly selectSavedOrDefaultModel?: (() => void) | undefined;
  readonly onShowParticipantsList?: (() => void) | undefined;
  readonly selectedVersionId?: string | undefined;
  readonly onSelectVersion: (version: VersionSummary) => void;
  readonly onShowVersionChangeAlert?: ((proceed: () => void) => void) | undefined;
  readonly onRefreshParticipantDetails?: (() => Promise<void> | void) | undefined;
  readonly variables: readonly AgentVariable[];
  readonly onChangeVariables: (variables: readonly AgentVariable[]) => void;
  readonly onShowAgentEditor?: ((participant: Participant) => void) | undefined;
  readonly onShowPipelineEditor?: ((participant: Participant) => void) | undefined;
  readonly onCloseAgentEditor?: (() => void) | undefined;
  readonly onClosePipelineEditor?: (() => void) | undefined;
}

export interface NewChatInputAttachmentsProps {
  readonly items?: readonly File[] | undefined;
  readonly onAttachFiles?: ((files: readonly File[]) => void) | undefined;
  readonly onDeleteAttachment?: ((index: number) => void) | undefined;
  readonly disabled?: boolean | undefined;
  readonly isUploading?: boolean | undefined;
  readonly uploadProgress?: number | undefined;
}

export interface NewChatInputMentionProps {
  readonly users?: readonly MentionCandidate[] | undefined;
  readonly onMentionChange?: ((mentions: readonly MentionMatch[]) => void) | undefined;
}

export interface NewChatInputVoiceProps {
  readonly isSpeakingMode?: boolean | undefined;
  readonly onSpeakingModeToggle?: (() => void) | undefined;
  readonly isTTSPlaying?: boolean | undefined;
  /** Externally-controlled one-shot mic-recording flag — see this file's module doc for why this replaces the baseline's own internally-owned `isRecording` state. */
  readonly isRecording?: boolean | undefined;
}

/** @public §3.5 budget: 10 top-level props (grouped). */
export interface NewChatInputProps {
  readonly conversationId?: string | undefined;
  readonly state?: NewChatInputStateProps | undefined;
  readonly content?: NewChatInputContentProps | undefined;
  readonly callbacks?: NewChatInputCallbacks | undefined;
  readonly agentEditor: NewChatInputAgentEditorProps;
  readonly attachments?: NewChatInputAttachmentsProps | undefined;
  readonly mentions?: NewChatInputMentionProps | undefined;
  readonly voice?: NewChatInputVoiceProps | undefined;
  readonly slots?: NewChatInputSlots | undefined;
  readonly refs?: NewChatInputRefs | undefined;
}
