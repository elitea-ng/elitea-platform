/**
 * Split out of `ChatBox.tsx` to stay under the file-length budget (§3.5) —
 * pure helper functions with no hooks/JSX: wire-shape participant
 * normalisation, `exactOptionalPropertyTypes`-safe object builders, and the
 * HITL child-thread-id derivation.
 */
import type { ComponentProps } from 'react';

import type { Participant } from '@/entities/participant';
import type { NewChatInput } from '@/features/chat-input';
import type { AnswerCanvasSelection, CanvasEditPayload, ChatMessageListCanvas, CodeBlockInfo } from '@/features/chat-messages';
import type { MessageGroupWire } from '@/entities/message';

import type { ChatBoxHandlerDeps } from './hooks/useChatBoxHandlers';
import type { UseChatBoxDataParams } from './hooks/useChatBoxData';
import type { UseChatBoxStateParams } from './hooks/useChatBoxState';

/** `ChatBox`'s `activeConversation` prop shape — defined here (not in `ChatBox.tsx`) so the several `ChatBox.helpers.ts` builder functions that read it (`buildChatBoxDataParams`/`buildChatBoxHandlerDeps`/etc.) don't need a reverse import back into `ChatBox.tsx`. */
export interface ChatBoxActiveConversation {
  readonly id?: string | number; readonly uuid?: string; readonly name?: string;
  readonly isNew?: boolean; readonly participants?: unknown[];
  readonly message_groups?: MessageGroupWire[];
  readonly isPlayback?: boolean; readonly isSending?: boolean;
  readonly meta?: Readonly<Record<string, unknown>>;
}

interface PendingHitlEntry {
  readonly tool_call_id?: string;
  readonly thread_id?: string;
  readonly child_thread_id?: string;
}

/** Derives `continueHitl`'s Track-2 `childThreadId` from the matching pending interrupt entry, by `toolCallId`. */
export function deriveHitlChildThreadId(pendingHitlMessage: { readonly hitlInterrupts?: readonly unknown[] | undefined } | undefined, toolCallId: string | undefined): string | undefined {
  if (!toolCallId) return undefined;
  const interrupts = pendingHitlMessage?.hitlInterrupts as readonly PendingHitlEntry[] | undefined;
  const matched = interrupts?.find((entry) => entry.tool_call_id === toolCallId);
  return matched?.thread_id || matched?.child_thread_id || undefined;
}

/** Builds the `getUserParticipant` result under `exactOptionalPropertyTypes`. */
export function buildUserParticipant(
  userId: string | undefined,
  userName: string | undefined,
  userAvatar: string | undefined,
): { id?: string; name?: string; avatar?: string } {
  return {
    ...(userId !== undefined ? { id: userId } : {}),
    ...(userName !== undefined ? { name: userName } : {}),
    ...(userAvatar !== undefined ? { avatar: userAvatar } : {}),
  };
}

/** Builds `createConversation`'s `{id, uuid}` result under `exactOptionalPropertyTypes`. */
export function pickIdAndUuid(created: { readonly id?: string | number; readonly uuid?: string }): {
  readonly id?: string | number;
  readonly uuid?: string;
} {
  return {
    ...(created.id !== undefined ? { id: created.id } : {}),
    ...(created.uuid !== undefined ? { uuid: created.uuid } : {}),
  };
}

/* ------------------------------------------------------------------ */
/* Raw wire participant -> `entities/participant`'s `Participant`.     */
/* `entities/participant`'s own normaliser (`lib/normalise.ts`) is not */
/* re-exported from that slice's barrel (already 20/20, §3.5 cap) and  */
/* R-L3 forbids a deep cross-slice import straight to it — a local     */
/* equivalent, same "duplicate small normalisation logic across a      */
/* layer boundary" precedent this codebase already uses repeatedly     */
/* (e.g. `isMcpToolkitType`, duplicated 4-5x across `features/chat-    */
/* input` for the identical reason).                                   */
/* ------------------------------------------------------------------ */
const PARTICIPANT_TYPES = new Set(['application', 'toolkit', 'llm', 'user', 'pipeline', 'skill', 'dummy']);

function readStr(record: Record<string, unknown> | null | undefined, key: string): string | undefined {
  const v = record?.[key];
  return typeof v === 'string' ? v : undefined;
}

/** Builds a `{[key]: value}` fragment only when `value` is defined — the `exactOptionalPropertyTypes`-safe way to conditionally set an optional field without ever assigning an explicit `undefined`. */
export function optField<K extends string, V>(key: K, value: V | undefined): { readonly [P in K]?: V } {
  return (value !== undefined ? { [key]: value } : {}) as { readonly [P in K]?: V };
}

/** `toParticipant`'s `entity_meta` sub-builder — extracted to keep `toParticipant`'s complexity down. */
function buildEntityMeta(entityMetaWire: Record<string, unknown> | null | undefined): Participant['entityMeta'] {
  if (!entityMetaWire) return undefined;
  return {
    ...optField('id', readStr(entityMetaWire, 'id')),
    ...optField('name', readStr(entityMetaWire, 'name')),
    ...optField('projectId', readStr(entityMetaWire, 'project_id')),
  };
}

/** `toParticipant`'s `meta` sub-builder — extracted to keep `toParticipant`'s complexity down. */
function buildMeta(metaWire: Record<string, unknown> | null | undefined): Participant['meta'] {
  if (!metaWire) return undefined;
  return {
    ...optField('id', readStr(metaWire, 'id')),
    ...optField('name', readStr(metaWire, 'name')),
    ...optField('userName', readStr(metaWire, 'user_name')),
    ...optField('userAvatar', readStr(metaWire, 'user_avatar')),
    ...optField('isContainer', typeof metaWire['is_container'] === 'boolean' ? metaWire['is_container'] : undefined), ...optField('mcp', typeof metaWire['mcp'] === 'boolean' ? metaWire['mcp'] : undefined),
  };
}

/** `toParticipant`'s `entity_settings` sub-builder — extracted to keep `toParticipant`'s complexity down. */
function buildEntitySettings(entitySettingsWire: Record<string, unknown> | null | undefined): Participant['entitySettings'] {
  if (!entitySettingsWire) return undefined;
  const versionIdWire = entitySettingsWire['version_id'];
  const versionId = typeof versionIdWire === 'string' || typeof versionIdWire === 'number' ? versionIdWire : undefined;
  const llmSettingsWire = entitySettingsWire['llm_settings'];
  return {
    ...optField('llmSettings', llmSettingsWire !== undefined && llmSettingsWire !== null ? (llmSettingsWire as Record<string, unknown>) : undefined),
    ...optField('versionId', versionId),
    ...('variables' in entitySettingsWire ? { variables: entitySettingsWire['variables'] } : {}),
    ...('icon_meta' in entitySettingsWire ? { iconMeta: entitySettingsWire['icon_meta'] } : {}),
    ...optField('toolkitType', readStr(entitySettingsWire, 'toolkit_type')),
    ...optField('agentType', readStr(entitySettingsWire, 'agent_type')), ...optField('mcpServerUrl', readStr(entitySettingsWire, 'mcp_server_url')),
  };
}
export function toParticipant(raw: unknown): Participant | undefined {
  if (!raw || typeof raw !== 'object') return undefined;
  const wire = raw as Record<string, unknown>;
  if (wire['id'] === undefined) return undefined;
  const entityNameRaw = wire['entity_name'];
  const entityName = (typeof entityNameRaw === 'string' && PARTICIPANT_TYPES.has(entityNameRaw) ? entityNameRaw : 'dummy') as Participant['entityName'];
  const idRaw = wire['id'];
  const id = typeof idRaw === 'string' ? idRaw : typeof idRaw === 'number' ? String(idRaw) : '';
  return {
    id,
    entityName,
    ...optField('entityMeta', buildEntityMeta(wire['entity_meta'] as Record<string, unknown> | null | undefined)),
    ...optField('meta', buildMeta(wire['meta'] as Record<string, unknown> | null | undefined)),
    ...optField('entitySettings', buildEntitySettings(wire['entity_settings'] as Record<string, unknown> | null | undefined)),
  };
}

export function toParticipants(raw: readonly unknown[] | undefined): Participant[] | undefined {
  if (!raw) return undefined;
  const result: Participant[] = [];
  for (const item of raw) {
    const p = toParticipant(item);
    if (p) result.push(p);
  }
  return result;
}

/** A `voiceHooks.useReadAloud()` result's TTS-sync fields, as `ChatMessageList`'s `tts` prop group expects them. */
export interface ChatBoxTtsProps {
  readonly autoSpeak: boolean;
  readonly onAutoSpeak: (text: string, messageId: string) => void;
  readonly speakingMessageId?: string | undefined;
  readonly speakingSegments?: readonly unknown[] | undefined;
  readonly spokenRange?: { readonly start: number; readonly end: number } | undefined;
}

/** Builds `ChatMessageList`'s `tts` prop group from a `voiceHooks.useReadAloud()` result — extracted to keep `ChatBox`'s complexity down. */
export function buildTtsProps(readAloud: {
  readonly onAutoSpeak: (text: string, messageId: string) => void;
  readonly speakingMessageId: string | number | null | undefined;
  readonly speakingSegments: readonly unknown[] | null | undefined;
  readonly spokenRange: { readonly start: number; readonly end: number } | null | undefined;
}): ChatBoxTtsProps {
  return {
    autoSpeak: false,
    onAutoSpeak: readAloud.onAutoSpeak,
    speakingMessageId: readAloud.speakingMessageId != null ? String(readAloud.speakingMessageId) : undefined,
    speakingSegments: readAloud.speakingSegments ?? undefined,
    spokenRange: readAloud.spokenRange ?? undefined,
  };
}

/** `ChatBox`'s input-disable/loading derivation — extracted to keep `ChatBox`'s own complexity down (a pure boolean-combination has no reason to live inside a component body). */
export function deriveChatBoxInputState(flags: {
  readonly isLoadingConversation: boolean | undefined;
  readonly isFetchingParticipantDetails: boolean;
  readonly isUploadingAttachments: boolean;
  readonly isUpdatingInternalToolsConfig: boolean;
  readonly isConversationSending: boolean | undefined;
  readonly isStreaming: boolean;
  readonly hasChatInput: boolean;
  readonly isProcessingSymbols: boolean;
  readonly hasPendingHitlInterrupt: boolean;
  readonly isActiveParticipantBroken: boolean;
}): { readonly isInputLoading: boolean; readonly disabledSend: boolean } {
  const isInputLoading =
    Boolean(flags.isLoadingConversation) ||
    flags.isFetchingParticipantDetails ||
    flags.isUploadingAttachments ||
    flags.isUpdatingInternalToolsConfig ||
    Boolean(flags.isConversationSending) ||
    flags.isStreaming;
  const disabledSend =
    !flags.hasChatInput ||
    isInputLoading ||
    flags.isProcessingSymbols ||
    flags.hasPendingHitlInterrupt ||
    flags.isActiveParticipantBroken;
  return { isInputLoading, disabledSend };
}

/** Flattens `ChatBox`'s grouped `user`/`llm`/`onDelete` props back to individual values — extracted to keep `ChatBox`'s own complexity down (each `?.` below is one fewer branch counted against the component). */
export function flattenChatBoxProps(props: {
  readonly user?: { readonly id?: string; readonly name?: string; readonly avatar?: string } | undefined;
  readonly llm?: { readonly settings?: Readonly<Record<string, unknown>>; readonly onSetSettings?: (settings: Readonly<Record<string, unknown>>) => void } | undefined;
  readonly onDelete?: { readonly answer?: (messageId: string) => void; readonly all?: () => void } | undefined;
}): {
  readonly userId: string | undefined;
  readonly userName: string | undefined;
  readonly userAvatar: string | undefined;
  readonly llmSettings: Readonly<Record<string, unknown>> | undefined;
  readonly onSetLLMSettings: ((settings: Readonly<Record<string, unknown>>) => void) | undefined;
  readonly onDeleteAnswer: ((messageId: string) => void) | undefined;
  readonly onDeleteAllMessages: (() => void) | undefined;
} {
  return {
    userId: props.user?.id,
    userName: props.user?.name,
    userAvatar: props.user?.avatar,
    llmSettings: props.llm?.settings,
    onSetLLMSettings: props.llm?.onSetSettings,
    onDeleteAnswer: props.onDelete?.answer,
    onDeleteAllMessages: props.onDelete?.all,
  };
}

type NewChatInputAgentEditorProps = ComponentProps<typeof NewChatInput>['agentEditor'];

/** `ChatBoxProps.editorCallbacks`'s shape — re-declared here (not imported) purely so this file's own exported builder function stays self-contained; `ChatBox.tsx` imports the canonical `ChatEditorCallbacks` type from `@/pages/chat` for its own prop declaration (structurally identical, checked, not duplicated logic). */
export interface ChatBoxEditorCallbacks {
  readonly onShowAgentEditor?: (participant: Participant) => void;
  readonly onShowPipelineEditor?: (participant: Participant) => void;
  readonly onCloseAgentEditor?: () => void;
  readonly onClosePipelineEditor?: () => void;
  /** "Create new" in the "+" menu (issue #867), read directly by `ChatBox.tsx` (not `resolveEditorCallbacks` — that feeds the EDIT-existing-participant slot). */ readonly onCreateAgent?: () => void; readonly onCreatePipeline?: () => void; readonly onCreateToolkit?: (isMcp?: boolean) => void;
  /** The transcript's canvas opener (issue 853), the block already open in the editor, and the CREATE gesture that carves a highlighted range out of an answer. Deliberately NOT resolved by `resolveEditorCallbacks` below — that result is spread into `NewChatInput`'s `agentEditor` props, and the composer knows nothing about a canvas. `buildCanvasProps` reads them instead. */
  readonly onShowCanvasEditor?: (payload: CanvasEditPayload) => void;
  readonly selectedCanvasBlock?: CodeBlockInfo | undefined;
  readonly onCreateCanvasFromSelection?: (payload: AnswerCanvasSelection) => void;
  readonly onOpenFileInCanvas?: (source: { readonly bucket: string; readonly name: string }) => void; // issue #878, same "not resolved below" reasoning
}

function noop(): void {}
/** `ChatMessageList`'s `canvas` group. Built here rather than inline: `ChatBox` is at its §3.5 complexity ceiling, and two more optional reads there breach it. */
export const buildCanvasProps = (editorCallbacks: ChatBoxEditorCallbacks | undefined): ChatMessageListCanvas => ({ onEdit: editorCallbacks?.onShowCanvasEditor, selected: editorCallbacks?.selectedCanvasBlock, onCreateFromSelection: editorCallbacks?.onCreateCanvasFromSelection, onOpenFile: editorCallbacks?.onOpenFileInCanvas });

/** The "+" menu's 3 CREATE callbacks (issue #867) — same reason `buildCanvasProps` is its own function: 3 more `?.` reads inline in `ChatBox` breach its complexity budget. */ export const buildCreateHandlerProps = (editorCallbacks: ChatBoxEditorCallbacks | undefined) => ({ onCreateAgent: editorCallbacks?.onCreateAgent, onCreatePipeline: editorCallbacks?.onCreatePipeline, onCreateToolkit: editorCallbacks?.onCreateToolkit });

/** Resolves `editorCallbacks`' 4 optional fields down to real-or-noop, extracted purely to keep `buildAgentEditorProps`'s own cyclomatic complexity under the oxlint budget (12) — 4 more `??` branches inline would have pushed it to 13. */
function resolveEditorCallbacks(editorCallbacks: ChatBoxEditorCallbacks | undefined): Required<Omit<ChatBoxEditorCallbacks, 'onShowCanvasEditor' | 'selectedCanvasBlock' | 'onCreateCanvasFromSelection' | 'onCreateAgent' | 'onCreatePipeline' | 'onCreateToolkit' | 'onOpenFileInCanvas'>> {
  return {
    onShowAgentEditor: editorCallbacks?.onShowAgentEditor ?? noop,
    onShowPipelineEditor: editorCallbacks?.onShowPipelineEditor ?? noop,
    onCloseAgentEditor: editorCallbacks?.onCloseAgentEditor ?? noop,
    onClosePipelineEditor: editorCallbacks?.onClosePipelineEditor ?? noop,
  };
}

/**
 * Builds `NewChatInput`'s `agentEditor` prop group — extracted to keep
 * `ChatBox`'s own complexity down. `onShowAgentEditor`/`onShowPipelineEditor`/
 * `onCloseAgentEditor`/`onClosePipelineEditor` fall back to their previous
 * literal no-ops when `editorCallbacks` (or an individual field within it)
 * isn't supplied — this keeps every existing `ChatBox` consumer/test working
 * unchanged (these were never required props) while routing to the real
 * `processes/chat/ui/ChatWithEditors.tsx`-supplied handlers once a caller
 * wires them. See that file's own module doc comment for the composition
 * root that actually supplies non-no-op callbacks.
 */
export function buildAgentEditorProps(params: {
  readonly participantForEditor: Participant | undefined;
  readonly activeParticipantDetails: NewChatInputAgentEditorProps['activeParticipantDetails'];
  readonly isAgentsPage: boolean | undefined;
  readonly selectSavedOrDefaultModel: NewChatInputAgentEditorProps['selectSavedOrDefaultModel'];
  readonly onShowParticipantsList: NewChatInputAgentEditorProps['onShowParticipantsList'];
  readonly onSelectVersion: NewChatInputAgentEditorProps['onSelectVersion'];
  readonly editorCallbacks: ChatBoxEditorCallbacks | undefined;
}): NewChatInputAgentEditorProps {
  const versionId = params.participantForEditor?.entitySettings?.versionId;
  const editorCallbacks = resolveEditorCallbacks(params.editorCallbacks);
  return {
    activeParticipant: params.participantForEditor,
    activeParticipantDetails: params.activeParticipantDetails,
    isAgentsPage: params.isAgentsPage ?? false,
    disableSwitchingParticipant: false,
    selectSavedOrDefaultModel: params.selectSavedOrDefaultModel,
    onShowParticipantsList: params.onShowParticipantsList,
    selectedVersionId: versionId !== undefined ? String(versionId) : undefined,
    onSelectVersion: params.onSelectVersion,
    variables: [],
    onChangeVariables: () => {},
    ...editorCallbacks,
  };
}

/** Builds `useChatBoxData`'s params from `ChatBox`'s raw props — extracted (the `?.`/`??` chains that read `activeConversation`'s optional fields move into this function's own complexity budget) to keep `ChatBox`'s own complexity down. */
export function buildChatBoxDataParams(params: {
  readonly activeConversation: ChatBoxActiveConversation | undefined;
  readonly activeParticipant: unknown;
  readonly projectId: string | number | undefined;
  readonly userId: string | undefined;
  readonly userName: string | undefined;
  readonly userAvatar: string | undefined;
  readonly isAgentsPage: boolean | undefined;
}): UseChatBoxDataParams {
  return {
    conversationId: params.activeConversation?.id,
    conversationUuid: params.activeConversation?.uuid,
    participants: params.activeConversation?.participants,
    messageGroups: params.activeConversation?.message_groups,
    activeParticipant: params.activeParticipant,
    projectId: params.projectId,
    userId: params.userId,
    userName: params.userName,
    userAvatar: params.userAvatar,
    isAgentsPage: params.isAgentsPage,
    conversationIsPlayingBack: Boolean(params.activeConversation?.isPlayback),
  };
}

/** Builds `useChatBoxState`'s params — extracted to keep `ChatBox`'s own complexity down (see `buildChatBoxDataParams`). */
export function buildChatBoxStateParams(params: {
  readonly activeParticipant: Participant | undefined;
  readonly participants: readonly Participant[] | undefined;
  readonly userId: string | undefined;
  readonly conversationStarters: readonly string[] | undefined;
  readonly isAgentsPage: boolean | undefined;
  readonly chatInput: UseChatBoxStateParams['chatInput'];
  readonly projectId: string | number | undefined;
  readonly activeParticipantVersions: UseChatBoxStateParams['activeParticipantVersions'];
}): UseChatBoxStateParams {
  return {
    activeParticipant: params.activeParticipant,
    participants: params.participants,
    userId: params.userId,
    conversationStarters: params.conversationStarters ?? [],
    isAgentsPage: params.isAgentsPage,
    chatInput: params.chatInput,
    projectId: params.projectId !== undefined ? String(params.projectId) : undefined,
    activeParticipantVersions: params.activeParticipantVersions,
  };
}

/** Builds `useChatBoxHandlers`'s dependency-injection object's `activeConversation`-derived fields — extracted to keep `ChatBox`'s own complexity down (see `buildChatBoxDataParams`). */
export function buildChatBoxHandlerConversationDeps(activeConversation: ChatBoxActiveConversation | undefined): {
  readonly participants: ChatBoxHandlerDeps['participants'];
  readonly conversationId: ChatBoxHandlerDeps['conversationId'];
  readonly conversationUuid: ChatBoxHandlerDeps['conversationUuid'];
} {
  return {
    participants: activeConversation?.participants,
    conversationId: activeConversation?.id,
    conversationUuid: activeConversation?.uuid,
  };
}

/** Every other field `ChatBox` itself reads off `activeConversation`/`projectId`, resolved once — extracted (with `buildChatBoxHandlerConversationDeps` above) so the `?.` chains that read `activeConversation`'s optional fields are counted once, here, rather than repeatedly against `ChatBox`'s own complexity budget. */
export function deriveChatBoxIds(activeConversation: ChatBoxActiveConversation | undefined, projectId: string | number | undefined): {
  readonly conversationId: string | number | undefined;
  readonly conversationParticipants: unknown[] | undefined;
  readonly conversationUuid: string | undefined;
  readonly conversationMeta: Readonly<Record<string, unknown>> | undefined;
  readonly isConversationSending: boolean | undefined;
  readonly projectIdString: string | undefined;
} {
  return {
    conversationId: activeConversation?.id,
    conversationParticipants: activeConversation?.participants,
    conversationUuid: activeConversation?.uuid,
    conversationMeta: activeConversation?.meta,
    isConversationSending: activeConversation?.isSending,
    projectIdString: projectId !== undefined ? String(projectId) : undefined,
  };
}

/** `ChatConversationStarters`'s `conversationStarters` prop — empty once a starter's been sent or the conversation has messages (baseline: `ChatBox.jsx`'s starters-visibility gating). Extracted to keep `ChatBox`'s own complexity down. */
export function resolveConversationStarters(
  hasStarterBeenSent: boolean,
  messageCount: number,
  conversationStarters: readonly string[] | undefined,
): readonly string[] | undefined {
  return hasStarterBeenSent || messageCount > 0 ? [] : conversationStarters;
}

/**
 * Whether the conversation surface's clear-history control refuses.
 *
 * The baseline's own `shouldDisableClear` (`!chat_history.length ||
 * isStreaming`): there is nothing to clear in an empty transcript, and
 * clearing mid-turn would delete rows the running turn is still writing.
 *
 * A function rather than an expression at the call site because `ChatBox` is
 * on its §3.5 complexity ceiling and this is one more branch there.
 */
export function shouldDisableClearChat(isStreaming: boolean, messageCount: number): boolean {
  return isStreaming || messageCount === 0;
}
