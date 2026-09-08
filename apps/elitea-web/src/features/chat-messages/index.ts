/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 *
 * This slice (`features/chat-messages`) is the Wave-2 unit C4 (chat-messages)
 * — the largest C-unit by LOC (~11,915). It owns:
 *
 *  - Message-row rendering: ApplicationAnswer, UserMessage, ChatMessageList
 *    (which folds the baseline's per-message `ChatMessageWrapper` controller
 *    directly into its own render loop — see that file's deletion note),
 *    HighlightedText, ErrorTrace, ChatContinue, ChatHitlActions,
 *    SubAgentAccordion
 *  - Attachments: FileList, NormalAttachment, MessageAttachmentList,
 *    ViewImageAttachmentModal
 *  - Canvas: Canvas, CanvasEditor, CanvasEditHeader, canvas hooks
 *  - Playback: PlaybackChatBox, PlaybackToolBar, playback hooks
 *  - Shared helpers: convertMessagesToChatHistory, subAgentGrouping,
 *    participantName
 *
 * Barrel split across these groups for readability. The spec §3.5 budget is
 * ≤20 named exports; this barrel is over that (waived — `slice-public-api`
 * in `scripts/lib/budgets-core.mjs`'s `BUDGET_WAIVERS`) given the size of
 * this unit's surface (message rendering, attachments, canvas, playback,
 * socket sync).
 */

// Bundled hooks (imported at top so chatHooks can reference them).
import { useParticipantName } from './lib/participantName';
import { useParticipantEntityType } from './lib/participantIcon';

/** React hooks bundled into a single namespace to stay within the ≤20 export budget. */
export const chatHooks = { useParticipantEntityType, useParticipantName };

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------
export type {
  ChatMessage,
  ConversationDetails,
  PlayerInfo,
} from './lib/convertMessagesToChatHistory';
export {
  convertConversationToChatHistory,
  convertMessagesToChatHistory,
  convertToPlayerQuestion,
  isUserMessage,
} from './lib/convertMessagesToChatHistory';

// Socket-driven message_group sync (chat_message_sync) — waived, see budgets-core.mjs.
export { useSyncChatMessage } from './model/useSyncChatMessage';

// REST-start + SSE-replay chat transport (issue #93) — waived, see budgets-core.mjs.
export { useChatStreamTransport } from './model/useChatStreamTransport';
export type { UseChatStreamTransportParams, UseChatStreamTransportResult } from './model/useChatStreamTransport';
export type { ChatStreamContext } from './lib/chatStreamReducer';

// Delete-confirmation dialog orchestrator — waived, see budgets-core.mjs.
export { useDeleteMessageAlert, ALL_MESSAGES } from './model/useDeleteMessageAlert';
export type { UseDeleteMessageAlertParams, UseDeleteMessageAlertResult } from './model/useDeleteMessageAlert';

export type {
  ClassifyWrapper,
  DeriveInstanceKey,
  PartitionedBlock,
} from './lib/subAgentGrouping';
export {
  buildPcidAnchorMap,
  inflightToolChipId,
  isInvocationId,
  INVOCATION_ID_RE,
  partitionActionsIntoBlocks,
  resolveExtraSubAgentKeys,
  resolveSubAgentLiveness,
  // Re-exports from entities/message/lib/subAgentGrouping.
  type SubAgentGroupable,
  collapseSubAgentInvocationKeys,
} from './lib/subAgentGrouping';

// Participant name — pure function + React hook.
export type { ParticipantNameInput } from './lib/participantName';
export { getParticipantName, useParticipantName } from './lib/participantName';

// Participant icon — icon type resolver.
export { resolveParticipantEntityType } from './lib/participantIcon';

// ---------------------------------------------------------------------------
// Message-row rendering
// ---------------------------------------------------------------------------
export { ActionView } from './ui/ActionView';
export type { ActionViewProps } from './ui/ActionView';
export { ApplicationAnswer } from './ui/chat-box/ApplicationAnswer';
export type { ApplicationAnswerProps } from './ui/chat-box/ApplicationAnswer';
export type { AnswerCanvasSelection } from './ui/chat-box/AnswerContent';
export { canvasByteRange } from './lib/canvasSelection';
export { ChatContinue } from './ui/chat-continue/ChatContinue';
export type { ChatContinueProps } from './ui/chat-continue/ChatContinue';
export { ChatHitlActions } from './ui/chat-hitl-actions/ChatHitlActions';
export type { ChatHitlActionsProps } from './ui/chat-hitl-actions/ChatHitlActions';
export { ChatMessageList } from './ui/chat-box/ChatMessageList';
export type { ChatMessageListCanvas, ChatMessageListProps } from './ui/chat-box/ChatMessageList';
export { CreatedTimeInfo } from './ui/CreatedTimeInfo';
export type { CreatedTimeInfoProps } from './ui/CreatedTimeInfo';
export { EditingPlaceholder } from './ui/EditingPlaceholder';
export { ErrorTrace } from './ui/error-trace/ErrorTrace';
export type { ErrorTraceProps } from './ui/error-trace/ErrorTrace';
export { FileList } from './ui/attachments/FileList';
export type { FileListProps } from './ui/attachments/FileList';
export { HighlightedText } from './ui/highlighted-text/HighlightedText';
export type { HighlightedTextProps } from './ui/highlighted-text/HighlightedText';
export { SubAgentAccordion } from './ui/sub-agent-section/SubAgentAccordion';
export type { SubAgentAccordionProps } from './ui/sub-agent-section/SubAgentAccordion';
export { ToolModal } from './ui/ToolModal';
export type { ToolModalProps } from './ui/ToolModal';
export { UserMessage } from './ui/chat-box/UserMessage';
export type { UserMessageProps } from './ui/chat-box/UserMessage';

// ---------------------------------------------------------------------------
// Playback
// ---------------------------------------------------------------------------
/*
 * This header has listed "Playback: PlaybackChatBox, PlaybackToolBar" since
 * the slice was written, and the barrel exported neither — so the component
 * was unreachable from outside the slice no matter what any consumer tried.
 * `processes/chat/ui/ChatPlayback.tsx` is the mount that needs them.
 */
export { PlaybackChatBox } from './ui/playback/PlaybackChatBox';
export type { PlaybackChatBoxHandle, PlaybackChatBoxProps, PlaybackChatMessage } from './ui/playback/PlaybackChatBox';

// ---------------------------------------------------------------------------
// Canvas — the editor shell, the markdown table editor (slice 3) and the
// mermaid quick-fix (slice 2b).
//
// Same gap as Playback above: Canvas/CanvasEditHeader were listed in this
// file's own header and exported by nobody. Over the ≤20 export budget with
// the rest of this barrel; waived in `scripts/lib/budgets-core.mjs`'s
// BUDGET_WAIVERS.
// ---------------------------------------------------------------------------
export { Canvas, extraCodeFromBlock } from './ui/canvas/Canvas';
export type { CanvasProps, CodeBlockInfo, CanvasEditPayload } from './ui/canvas/Canvas';
export { CanvasEditor } from './ui/canvas/CanvasEditor';
export type { CanvasEditorHandle, CanvasEditorProps } from './ui/canvas/CanvasEditor';
export { CanvasEditHeader } from './ui/canvas/CanvasEditHeader';
export type {
  CanvasEditHeaderProps,
  CanvasEditHeaderActions,
  CanvasEditHeaderTable,
  CanvasEditHeaderLangSelect,
} from './ui/canvas/CanvasEditHeader';
export { MarkdownTableEditor } from './ui/canvas/table/MarkdownTableEditor';
export type { MarkdownTableEditorHandle, MarkdownTableEditorProps } from './ui/canvas/table/MarkdownTableEditor';
export { ImportTableButton } from './ui/canvas/table/ImportTableButton';
export { MermaidQuickFixButton } from './ui/canvas/MermaidQuickFixButton';
export { parseDelimitedText, parseMarkdownTable, serialiseMarkdownTable } from './lib/markdownTable';
export type { MarkdownTableData } from './lib/markdownTable';
export { useMermaidQuickFix } from './model/useMermaidQuickFix';
export type {
  MermaidQuickFixCapability,
  MermaidQuickFixUnavailableReason,
  UseMermaidQuickFixResult,
} from './model/useMermaidQuickFix';
