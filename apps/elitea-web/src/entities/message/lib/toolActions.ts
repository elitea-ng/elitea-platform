/**
 * `convertToAIAnswer`'s `toolActions[]` assembly
 * (apps/elitea-ui/src/common/convertChatConversationMessages.js:141-263),
 * ported verbatim and split into small functions to stay under the repo's
 * cyclomatic-complexity budget (.oxlintrc.json "complexity": 12). Inner
 * field names (`toolInputs`/`toolOutputs`/`toolMeta`/`created_at`/`ended_at`/
 * `parent_agent_name`/…) are kept EXACTLY as the source wrote them — a
 * mix of camelCase and snake_case — because `ToolAction`
 * (model/types.ts) is a deliberately loose `{ type; timestamp?;
 * [key: string]: unknown }` catch-all and a Wave-2 chat renderer is
 * expected to read these under their historical keys, not a re-normalised
 * set.
 */
import { TOOL_ACTION_NAMES, TOOL_ACTION_TYPES, ToolActionStatus } from '@/shared/lib/chat';
import { convertJsonToString } from '@/shared/lib/json';

import { collapseSubAgentInvocationKeys } from './subAgentGrouping';
import type { PersistedTraceSteps } from './traceSteps';
import type {
  MessageGroupWire,
  MessageParticipantToolWire,
  MessageParticipantWire,
  ThinkingStepWire,
  ToolCallStepWire,
} from './wire';

/**
 * `meta.thinking_steps`/`meta.tool_calls`/`meta.first_tool_timestamp_start` —
 * `buildToolActions`' raw inputs.
 *
 * `persisted` is the trace-step fallback (#951): the conversation read stopped
 * carrying tool detail in `meta` when the trace-step migration landed, so a
 * RELOADED turn has an empty `meta` and its steps live in
 * `chat_message_trace_step` instead. Taken only when `meta` carries NEITHER
 * kind, so a live turn and a pre-migration conversation both keep what they
 * had, and the two sources are never interleaved.
 */
export function resolveAssistantToolInputs(
  meta: MessageGroupWire['meta'],
  persisted: PersistedTraceSteps | undefined,
) {
  const own = { toolCalls: meta?.tool_calls ?? {}, thinkingSteps: meta?.thinking_steps ?? [] };
  const source = countSteps(own.toolCalls) === 0 && own.thinkingSteps.length === 0 ? (persisted ?? own) : own;
  return { ...source, firstToolTimestampStart: meta?.first_tool_timestamp_start };
}

/** `meta.tool_calls` is a map on one producer and an array on another. */
function countSteps(toolCalls: Readonly<Record<string, ToolCallStepWire>> | readonly ToolCallStepWire[]): number {
  return Array.isArray(toolCalls) ? toolCalls.length : Object.keys(toolCalls).length;
}

/**
 * One built tool-action entry, pre-`ToolAction`-cast (mutable during
 * grouping). The `| undefined` widenings (vs. simply omitting the key) on
 * `id`/`parent_agent_name`/`parent_agent_call_id` are required by
 * `exactOptionalPropertyTypes` — the source genuinely produces `undefined`
 * for these (e.g. `step.tool_run_id` may be absent), not just "sometimes
 * present".
 */
export interface ToolActionDraft {
  type: string;
  name?: string;
  parent_agent_name?: string | null | undefined;
  original_name?: string | null;
  parent_agent_call_id?: string | undefined;
  id?: string | undefined;
  status: string;
  toolInputs?: unknown;
  toolOutputs?: unknown;
  toolMeta?: Record<string, unknown>;
  /** See `traceStepIdentity` — set only on a step rebuilt from a trace row. */
  traceStepId?: number;
  traceMessageGroupId?: number;
  created_at: string;
  ended_at: string;
  timestamp: string;
  content?: string;
  thinking?: unknown;
  isError?: boolean;
  [key: string]: unknown;
}

/**
 * The persisted row a restored step came from, carried onto the draft so
 * `ToolModal` can fetch that one row's heavy columns when the pin is opened
 * (#951). Absent on every live step.
 */
function traceStepIdentity(
  step: { readonly trace_step_id?: number; readonly trace_message_group_id?: number },
): { traceStepId?: number; traceMessageGroupId?: number } {
  if (step.trace_step_id === undefined || step.trace_message_group_id === undefined) return {};
  return { traceStepId: step.trace_step_id, traceMessageGroupId: step.trace_message_group_id };
}

type SortableStep =
  | (ThinkingStepWire & { readonly stepType: 'thinking_step' })
  | (ToolCallStepWire & { readonly stepType: 'tool_call' });

/** `sortedSteps` (lines 141-150): chronological merge of both step kinds. */
function sortSteps(
  thinkingSteps: readonly ThinkingStepWire[],
  toolCallSteps: readonly ToolCallStepWire[],
): SortableStep[] {
  const steps: SortableStep[] = [
    ...thinkingSteps.map((step) => ({ ...step, stepType: 'thinking_step' as const })),
    ...toolCallSteps.map((step) => ({ ...step, stepType: 'tool_call' as const })),
  ];
  return steps.sort(
    (a, b) =>
      // convertChatConversationMessages.js:147-149 uses `||`, not `??` — an
      // empty-string timestamp_start (falsy-but-defined) must also fall
      // through to timestamp_finish, matching every sibling fallback in this
      // file (resolveActionCreatedAt, resolveToolkitName, resolveToolkitType,
      // resolveToolActionName all use `||` for the same reason).
      new Date(a.timestamp_start || a.timestamp_finish || '').getTime() -
      new Date(b.timestamp_start || b.timestamp_finish || '').getTime(),
  );
}

/** chat.helpers.js:58-71 `getToolActionOriginalName`, ported verbatim. */
function getToolActionOriginalName(metadata: ToolCallStepWire['metadata']): string | null {
  if (metadata?.toolkit_type === 'internal') return null;
  if (metadata?.original_name) return metadata.original_name;
  const ns = metadata?.checkpoint_ns;
  if (!ns) return null;
  const name = ns.split(':')[0];
  return name && name !== 'main_agent' && name !== 'agent' ? name : null;
}

/** `message.response_metadata.{model_name,tool_name}` (lines 156-160), defaulted to `''`. */
function resolveLlmResponseMetadata(step: ThinkingStepWire): { modelName: string; toolName: string } {
  const responseMetadata = step.message?.response_metadata;
  return { modelName: responseMetadata?.model_name ?? '', toolName: responseMetadata?.tool_name ?? '' };
}

/** `created_at` fallback chain shared by both branches (lines 181-184, 237-240). */
function resolveActionCreatedAt(
  gatedFirstTimestampStart: string | undefined,
  timestampStart: string | undefined,
  timestampFinish: string | undefined,
  fallbackCreatedAt: string,
): string {
  return gatedFirstTimestampStart || timestampStart || timestampFinish || fallbackCreatedAt;
}

/**
 * The LLM/thinking-step branch (lines 152-191); `undefined` when skipped
 * (167-169).
 *
 * The text gate is the source's, with one exception: a step REBUILT from a
 * persisted trace row (`trace_step_id`) is kept even with no text, because the
 * listing that produced it has already dropped the blank ones
 * (`has_visible_content` — `messagetraces/handler.go`) and the text it does
 * have is a detail-only column the modal fetches on open (#951). Dropping it
 * here would silently lose a step the API says exists.
 */
function buildLlmToolAction(
  step: ThinkingStepWire,
  fallbackCreatedAt: string,
  gatedFirstTimestampStart: string | undefined,
): ToolActionDraft | undefined {
  const text = step.text;
  if ((!text || !text.trim()) && step.trace_step_id === undefined) return undefined;
  const { modelName, toolName } = resolveLlmResponseMetadata(step);
  const endedAt = step.timestamp_finish || fallbackCreatedAt;
  return {
    ...traceStepIdentity(step),
    type: TOOL_ACTION_TYPES.Llm,
    name: toolName || TOOL_ACTION_NAMES.Llm,
    parent_agent_name: step.parent_agent_name || null,
    id: step.message?.id,
    status: ToolActionStatus.complete,
    toolInputs: '',
    toolOutputs: text ?? '',
    toolMeta: { ls_model_name: modelName },
    created_at: resolveActionCreatedAt(gatedFirstTimestampStart, step.timestamp_start, step.timestamp_finish, fallbackCreatedAt),
    ended_at: endedAt,
    timestamp: endedAt,
    content: text ?? '',
    thinking: step.thinking,
  };
}

/** `toolkit_name` resolution (lines 196-200). */
function resolveToolkitName(step: ToolCallStepWire, toolNameRaw: string): string {
  const hasOldFormat = toolNameRaw.includes('___');
  return (
    step.toolkit_name ||
    step.tool_meta?.metadata?.toolkit_name ||
    (hasOldFormat ? (toolNameRaw.split('___')[0] ?? '') : '')
  );
}

/** `toolkit_type` resolution (lines 201-206), including the participant `tools[]` fallback lookup. */
function resolveToolkitType(
  step: ToolCallStepWire,
  toolkitName: string,
  tools: readonly MessageParticipantToolWire[] | undefined,
): string {
  return (
    step.toolkit_type ||
    step.tool_meta?.metadata?.toolkit_type ||
    step.metadata?.toolkit_type ||
    tools?.find((tool) => tool.name === toolkitName || tool.toolkit_name === toolkitName)?.type ||
    ''
  );
}

/** `name` field resolution (lines 211-214). */
function resolveToolActionName(step: ToolCallStepWire): string {
  if (step.metadata?.original_name && step.tool_meta?.name) return step.tool_meta.name;
  return step.tool_name || step.name || 'Tool Call';
}

/** `toolMeta` object (lines 225-236). */
function buildToolCallMeta(
  step: ToolCallStepWire,
  toolkitName: string,
  toolkitType: string,
): Record<string, unknown> {
  return {
    ls_model_name: step.tool_meta?.model_name,
    toolkit_name: toolkitName,
    display_name: step.tool_meta?.display_name || step.tool_meta?.metadata?.display_name,
    toolkit_type: toolkitType,
    mcp_server_url: step.metadata?.mcp_server_url,
    langgraph_node: step.metadata?.langgraph_node,
    icon_meta: step.tool_meta?.icon_meta,
    agent_type: step.tool_meta?.metadata?.agent_type,
  };
}

/** The tool-call branch (lines 192-246). */
function buildToolCallAction(
  step: ToolCallStepWire,
  fallbackCreatedAt: string,
  gatedFirstTimestampStart: string | undefined,
  tools: readonly MessageParticipantToolWire[] | undefined,
): ToolActionDraft {
  const toolNameRaw = step.tool_name || step.name || '';
  const toolkitName = resolveToolkitName(step, toolNameRaw);
  const toolkitType = resolveToolkitType(step, toolkitName, tools);
  const endedAt = step.timestamp_finish || fallbackCreatedAt;
  return {
    ...traceStepIdentity(step),
    type: TOOL_ACTION_TYPES.Tool,
    name: resolveToolActionName(step),
    original_name: getToolActionOriginalName(step.metadata),
    parent_agent_name: step.metadata?.parent_agent_name,
    parent_agent_call_id: step.metadata?.parent_agent_call_id,
    id: step.tool_run_id,
    status: ToolActionStatus.complete,
    toolInputs: step.tool_inputs,
    toolOutputs: step.tool_output,
    toolMeta: buildToolCallMeta(step, toolkitName, toolkitType),
    created_at: gatedFirstTimestampStart || step.timestamp_start || fallbackCreatedAt,
    ended_at: endedAt,
    timestamp: endedAt,
    content: step.content || convertJsonToString(step.error ?? ''),
    isError: Boolean(step.error),
  };
}

/** `collapseSubAgentInvocationKeys`'s call-site predicate (lines 257-262). */
function isWrapperCompletion(action: ToolActionDraft, name: string): boolean {
  return (
    action.type === TOOL_ACTION_TYPES.Tool &&
    !action.parent_agent_name &&
    (action.name === name || action.original_name === name) &&
    !action.isError &&
    !!action.toolOutputs
  );
}

/**
 * `convertToAIAnswer` lines 141-263: builds, sorts and de-flickers a
 * message's `toolActions[]` from its persisted `meta.thinking_steps`/
 * `meta.tool_calls`.
 */
export function buildToolActions(
  thinkingSteps: readonly ThinkingStepWire[],
  rawToolCalls: Readonly<Record<string, ToolCallStepWire>> | readonly ToolCallStepWire[],
  fallbackCreatedAt: string,
  firstToolTimestampStart: string | undefined,
  participant: MessageParticipantWire | undefined,
): ToolActionDraft[] {
  const toolCallSteps = Array.isArray(rawToolCalls) ? rawToolCalls : Object.values(rawToolCalls);
  const tools = participant?.meta?.tools;
  const toolActions: ToolActionDraft[] = [];
  sortSteps(thinkingSteps, toolCallSteps).forEach((step, index) => {
    const gated = index === 0 ? firstToolTimestampStart : undefined;
    if (step.stepType === 'thinking_step') {
      const action = buildLlmToolAction(step, fallbackCreatedAt, gated);
      if (action) toolActions.push(action);
    } else {
      toolActions.push(buildToolCallAction(step, fallbackCreatedAt, gated, tools));
    }
  });
  return collapseSubAgentInvocationKeys(toolActions, {
    deriveName: (action) => action.parent_agent_name || action.original_name || '',
    deriveRawKey: (action) => action.parent_agent_call_id || '',
    isWrapperCompletion,
  });
}
