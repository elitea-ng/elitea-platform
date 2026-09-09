/**
 * Split out of `ChatBox.tsx` to stay under the file-length/complexity
 * budgets (§3.5) — `NewChatInput`'s `slots`/`refs` prop bundles (send
 * control, highlight overlay, attachment button, internal-tools-config
 * button, voice button, LLM model selector), grouped into option objects
 * the same way `ChatBoxPopups` groups its own props.
 *
 * `sendControl`/`highlightOverlay`/the attachment+voice imperative refs
 * were a disclosed open gap (see `NewChatInput.types.ts`'s module doc) —
 * this is that composition-root wiring landing. `SendButton`'s own props
 * are a strict subset of `UserInputSendControlSlotProps` (it doesn't
 * consume `config`), so the slot function just forwards the rest;
 * `HighlightedText`'s `{text, ranges}` match `UserInputHighlightOverlaySlotProps`
 * exactly.
 */
import type { ComponentProps, ReactNode, RefObject } from 'react';

import { AttachmentButton, ChatInternalToolsConfigButton, ClearChatButton, PlusChatButton, SendButton, VoiceButton } from '@/widgets/chat';
import type { AttachmentButtonHandle, PlusChatButtonEntitySubmenus, VoiceButtonHandle, VoiceButtonInputHandle } from '@/widgets/chat';
import { FileList, HighlightedText } from '@/features/chat-messages';
import { NewChatInput } from '@/features/chat-input';
import { LLMModelSelector } from '@/widgets/llm-model-selector';
import { t } from '@/shared/i18n';

import { optField } from './ChatBox.helpers';

/** `NewChatInputSlots`/its two slot-prop types stay unexported from `features/chat-input`'s barrel (same convention as `NewChatInputHandle`/`NewChatInputProps`) — derived the same way. */
type NewChatInputSlotsType = NonNullable<ComponentProps<typeof NewChatInput>['slots']>;
type SendControlSlotProps = Parameters<NonNullable<NewChatInputSlotsType['sendControl']>>[0];
type HighlightOverlaySlotProps = Parameters<NonNullable<NewChatInputSlotsType['highlightOverlay']>>[0];
type AttachmentListSlotProps = Parameters<NonNullable<NewChatInputSlotsType['attachmentList']>>[0];

type LLMSettingsValues = NonNullable<ComponentProps<typeof LLMModelSelector>['llmSettings']>;
type LLMModelListItem = NonNullable<ComponentProps<typeof LLMModelSelector>['models']>[number];
type InternalToolItem = NonNullable<ComponentProps<typeof ChatInternalToolsConfigButton>['tools']>[number];

interface ChatBoxInputSlotsAttachments {
  readonly attachments: ComponentProps<typeof AttachmentButton>['attachments'] | undefined;
  readonly onAttachFiles: ComponentProps<typeof AttachmentButton>['onAttachFiles'] | undefined;
}

interface ChatBoxInputSlotsInternalTools {
  readonly disabled: boolean;
  readonly tools: readonly InternalToolItem[];
  readonly onToolChange: ComponentProps<typeof ChatInternalToolsConfigButton>['onToolChange'] | undefined;
}

interface ChatBoxInputSlotsModel {
  readonly llmSettings: LLMSettingsValues | undefined;
  readonly onSetLLMSettings: ((settings: Readonly<Record<string, unknown>>) => void) | undefined;
  readonly selectedModel: ComponentProps<typeof LLMModelSelector>['selectedModel'];
  readonly onSelectModel: ComponentProps<typeof LLMModelSelector>['onSelectModel'];
  readonly models: readonly LLMModelListItem[];
}

/**
 * The conversation-surface "clear the history" control (gap G1).
 *
 * The mechanism behind it was complete and unreachable: `ChatBox` exposes
 * `onClear` on its imperative handle, `useChatBoxActions.handleClear` opens
 * the delete-all confirmation, `useDeleteMessageAlert`'s `ALL_MESSAGES`
 * sentinel routes the confirm to `clearChat`, and `clearChat` issues
 * `DELETE /elitea_core/messages/prompt_lib/{projectId}/{conversationId}`.
 * The ONLY caller was the pipeline editor's test-chat panel, which reaches
 * the handle through a ref. On `/chat` nothing called it, so a user could
 * empty a conversation only by deleting one message at a time or deleting
 * the conversation itself. Both halves were correct; the composition was
 * missing.
 *
 * `disabled` reproduces the baseline's own `shouldDisableClear`
 * (`!chat_history.length || isStreaming`): there is nothing to clear in an
 * empty transcript, and clearing mid-turn would delete rows the running turn
 * is still writing.
 */
interface ChatBoxInputSlotsClearChat {
  readonly disabled: boolean;
  readonly onClear: () => void;
}

interface ChatBoxInputSlotsRefs {
  readonly attachmentButtonRef: RefObject<AttachmentButtonHandle | null>;
  readonly voiceButtonRef: RefObject<VoiceButtonHandle | null>;
  readonly voiceInputRef: RefObject<VoiceButtonInputHandle | null>;
}

export interface ChatBoxInputSlotsProps {
  readonly attachments: ChatBoxInputSlotsAttachments;
  readonly internalTools: ChatBoxInputSlotsInternalTools;
  readonly model: ChatBoxInputSlotsModel;
  readonly clearChat: ChatBoxInputSlotsClearChat;
  readonly refs: ChatBoxInputSlotsRefs;
  /**
   * The baseline's `fromTheChat` — true on the chat surface, false when the
   * ChatBox is embedded in the agents page. It selects WHICH left-hand
   * control the footer gets: the "+" menu, or a bare paperclip.
   */
  readonly isAgentsPage: boolean;
  /** Real agent/pipeline/toolkit/MCP lists for the "+" menu's submenus, from `processes/chat`. */
  readonly entitySubmenus: PlusChatButtonEntitySubmenus | undefined;
  /** Participants list the "+" menu's Agents submenu picks from. */
  readonly participants: readonly unknown[] | undefined;
  /** "Create new" in the "+" menu's Agents/Pipelines/Toolkits submenus (issue #867) — `undefined` on the agents-page surface, which renders no "+" menu at all. */
  readonly onCreateAgent?: (() => void) | undefined;
  readonly onCreatePipeline?: (() => void) | undefined;
  readonly onCreateToolkit?: ((isMcp?: boolean) => void) | undefined;
}

export interface ChatBoxInputSlotsResult {
  readonly sendControl: (props: SendControlSlotProps) => ReactNode;
  readonly highlightOverlay: (props: HighlightOverlaySlotProps) => ReactNode;
  /** The staged-file chips above the text area — `UserInput` invokes it only when a file is staged. */
  readonly attachmentList: (props: AttachmentListSlotProps) => ReactNode;
  readonly attachmentButton: ReactNode;
  /** `undefined` on the chat surface — the "+" menu owns the modules there. */
  readonly internalToolsConfig: ReactNode;
  /** `undefined` on the agent/pipeline editor surface, which renders its own. */
  readonly clearChat: ReactNode;
  readonly voiceButton: ReactNode;
  readonly modelSelector: ReactNode;
}

/**
 * The composer's `attachments` PROP bundle — the staged files it shows, as
 * opposed to the slot that draws them.
 *
 * Its own function, and in this file rather than at the call site, for the
 * reason the whole file exists: `ChatBox.tsx` sits on the §3.5 400-line
 * ceiling. It is also where the defect was. `ChatBox` held the attachment
 * state, handed it to the "+" menu (whose capacity counter ticked down) and to
 * the send path (which uploaded it), and passed NO `attachments` prop to
 * `NewChatInput` — so `resolveAttachments` defaulted `items` to `[]` and the
 * composer showed a picked file to nobody. `isUploading`/`uploadProgress` were
 * missing the same way, which is what left `UploadProgressIndicator`'s
 * determinate variant unreachable.
 */
export function buildChatBoxAttachmentProps(attachments: {
  readonly state: { readonly attachments: readonly File[]; readonly onAttachFiles: (files: readonly File[]) => void; readonly onDeleteAttachment: (index: number) => void };
  readonly upload: { readonly isUploading: boolean; readonly uploadProgress: number };
}): NonNullable<ComponentProps<typeof NewChatInput>['attachments']> {
  return {
    items: attachments.state.attachments,
    onAttachFiles: attachments.state.onAttachFiles,
    onDeleteAttachment: attachments.state.onDeleteAttachment,
    isUploading: attachments.upload.isUploading,
    uploadProgress: attachments.upload.uploadProgress,
  };
}

/** `sendControl`'s props depend on its own per-invocation callback argument, so (unlike the other slot props below) they can't be precomputed eagerly — this stays a plain, non-JSX helper (same reasoning as this file's other prop builders: no JSXElement ancestor, so `optField`'s string-literal keys aren't parsed as JSX-nested literals by the `i18next/no-literal-string` gate). */
function buildSendButtonProps(props: SendControlSlotProps) {
  return {
    isSpeakingMode: props.isSpeakingMode,
    question: props.question,
    disabledSend: props.disabledSend,
    onSend: props.onSend,
    ...optField('onEnterSpeakingMode', props.onEnterSpeakingMode),
    ...optField('onExitSpeakingMode', props.onExitSpeakingMode),
    ...optField('tooltipOfSendButton', props.tooltipOfSendButton),
  };
}

/** Builds `NewChatInput`'s `slots` prop bundle — a function (not a component) so its return type slots directly into `NewChatInput`'s `slots` prop without an extra wrapper element. Prop objects are built as local consts (not inline `{...optField(...)}` spreads) so their `optField`-derived string keys aren't parsed as JSX-nested literals by the `i18next/no-literal-string` gate. */
export function buildChatBoxInputSlots({
  attachments,
  internalTools,
  model,
  clearChat,
  refs,
  isAgentsPage,
  entitySubmenus,
  participants,
  onCreateAgent,
  onCreatePipeline,
  onCreateToolkit,
}: ChatBoxInputSlotsProps): ChatBoxInputSlotsResult {
  const attachmentButtonProps = {
    disableAttachments: false,
    ...optField('attachments', attachments.attachments),
    ...optField('onAttachFiles', attachments.onAttachFiles),
  };
  const internalToolsConfigProps = {
    disabled: internalTools.disabled,
    tools: [...internalTools.tools],
    ...optField('onToolChange', internalTools.onToolChange),
  };
  const modelSelectorProps = {
    ...optField('llmSettings', model.llmSettings),
    ...optField('onSetLLMSettings', model.onSetLLMSettings ? (settings: LLMSettingsValues) => model.onSetLLMSettings?.(settings as Readonly<Record<string, unknown>>) : undefined),
    ...optField('selectedModel', model.selectedModel),
    ...optField('onSelectModel', model.onSelectModel),
    models: [...model.models],
    showStepsLimit: true,
  };
  // `ChatInternalToolsConfigButton` and `PlusChatButton` take the same data in
  // two different shapes — a `{key,label,enabled}` list plus
  // `(key, enabled)`, versus enabled NAMES plus `({key, value})`. `key` here
  // IS the tool's `name` (`useChatBoxInternalTools` builds it from
  // `tool.name`), so the translation is shape-only, not a re-keying.
  const onToolChange = internalTools.onToolChange;
  const clearChatProps = {
    disabled: clearChat.disabled,
    onClear: clearChat.onClear,
    label: t('widgets.chatBox.clearHistoryLabel', 'Clear the chat history'),
    testId: 'chat-clear-history',
  };
  const plusButtonProps = {
    ...attachmentButtonProps,
    // The drop/paste bridge (`useNewChatInputAttachmentBridge`) delivers files
    // through `refs.attachmentButtonRef.current.onDrop(...)` and silently
    // no-ops while `current` is null — so on the chat surface the ref must
    // reach `PlusChatButton`'s hidden always-mounted `AttachmentButton`
    // (baseline `NewChatInput.jsx:240-241`), exactly as the agents-page branch
    // below attaches it to the bare paperclip.
    attachmentButtonRef: refs.attachmentButtonRef,
    ...optField(
      'onInternalToolsConfigChange',
      onToolChange ? ({ key, value }: { key: string; value: boolean }) => { onToolChange(key, value); } : undefined,
    ),
    internal_tools: internalTools.tools.filter((tool) => tool.enabled).map((tool) => tool.key),
    ...optField('entitySubmenus', entitySubmenus),
    ...optField('participants', participants ? [...participants] : undefined),
    ...optField('onCreateAgent', onCreateAgent),
    ...optField('onCreatePipeline', onCreatePipeline),
    ...optField('onCreateToolkit', onCreateToolkit),
  };
  return {
    sendControl: (props: SendControlSlotProps) => <SendButton {...buildSendButtonProps(props)} />,
    highlightOverlay: (props: HighlightOverlaySlotProps) => <HighlightedText text={props.text} ranges={props.ranges} />,
    // The chips for the files staged on the next message. `UserInput` calls
    // this slot only when there IS at least one staged file, so the empty
    // case needs no branch here (`renderAttachmentList`).
    attachmentList: (props: AttachmentListSlotProps) => (
      <FileList
        attachments={props.attachments}
        onDeleteAttachment={props.onDeleteAttachment}
        disabled={props.disabled}
      />
    ),
    // Baseline `NewChatInput.jsx:239-274`: on the chat surface the "+" menu IS
    // the left-hand control and it SUBSUMES both the paperclip and the
    // internal-tools gear (its own "Attach Files" row and "Modules" submenu).
    // Rendering all three side by side — which is what this file did, because
    // nothing ever mounted `PlusChatButton` — gave the composer two buttons
    // where the product has one, and left the entity submenus (agents,
    // pipelines, toolkits, MCPs) unreachable from the chat entirely.
    attachmentButton: isAgentsPage
      ? <AttachmentButton ref={refs.attachmentButtonRef} {...attachmentButtonProps} />
      : <PlusChatButton {...plusButtonProps} />,
    // `undefined` on the chat surface: `NewChatInputFooterContent` gates this
    // slot on `!isAgentsPage`, so returning the gear here would render it
    // BESIDE the "+" that already contains it. The composition root is what
    // withholds it, exactly as that component's own doc comment specifies.
    internalToolsConfig: isAgentsPage ? <ChatInternalToolsConfigButton {...internalToolsConfigProps} /> : undefined,
    // The mirror image of the line above, and withheld for the same kind of
    // reason: the agent/pipeline editor surface already renders a clear
    // control of its own beside the panel (`features/pipelines/ui/
    // ChatPanel.tsx`'s `renderClearChatButton`), so supplying one here would
    // put two on the same screen.
    clearChat: isAgentsPage ? undefined : <ClearChatButton {...clearChatProps} />,
    voiceButton: <VoiceButton ref={refs.voiceButtonRef} inputRef={refs.voiceInputRef} disabled={false} onRecordingChange={() => {}} />,
    modelSelector: <LLMModelSelector {...modelSelectorProps} />,
  };
}
