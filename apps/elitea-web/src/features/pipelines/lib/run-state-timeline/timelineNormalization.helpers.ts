/**
 * Pure run-event & timeline normalization helpers (`EL-3734` / `#1585` +
 * replatform Map/Parallel/loop/nested-application grouping).
 *
 * Transforms streamed `TimelineRunSocketEvent[]` (including
 * `agent_on_transitional_edge`, LLM/tool/HITL events, nested application
 * hierarchies, Map/Parallel children, and loop iterations) into
 * `GroupedTimelineStep[]` ready for `TimeLine`.
 */
import { PipelineStatus } from '../flow-editor/constants/flowEditor.constants';
import {
  PIPELINE_SOCKET_MESSAGE_TYPE,
  toolkitNameFromRawToolName,
} from '../flow-editor/helpers/parseRunsByEvent.support';

import {
  MAX_HIERARCHY_CALL_ID_LENGTH,
  MAX_HIERARCHY_DEPTH,
  MAX_HIERARCHY_NAME_LENGTH,
} from './timeline.constants';
import type {
  GroupedTimelineStep,
  TimelineAgentPathEntry,
  TimelineEventMetadata,
  TimelineRunSocketEvent,
  TimelineStepKind,
} from './timeline.types';

interface NormalizedHierarchy {
  readonly path: readonly TimelineAgentPathEntry[];
  readonly owner: TimelineAgentPathEntry | undefined;
  readonly scope: TimelineAgentPathEntry | undefined;
}

interface MutableGroupedStep {
  id: string;
  langgraph_node?: string | undefined;
  langgraph_checkpoint_ns?: string | undefined;
  status?: string | undefined;
  state?: Readonly<Record<string, unknown>> | undefined;
  created_at: number | string;
  tool_run_id?: string | undefined;
  source?: string | undefined;
  target?: string | undefined;
  error?: string | undefined;
  group_id?: string | undefined;
  scope_id?: string | undefined;
  group_label?: string | undefined;
  is_nested?: boolean | undefined;
  step_kind?: TimelineStepKind | undefined;
  iteration_index?: number | undefined;
  sibling_ordinal?: number | undefined;
  parent_agent_path?: readonly TimelineAgentPathEntry[] | undefined;
}

function asBoundedString(value: unknown, maxLength: number): string | undefined {
  if (typeof value !== 'string' || value.length === 0 || value.length > maxLength) return undefined;
  return value;
}

function asFiniteNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function parseAgentPathEntry(raw: unknown): TimelineAgentPathEntry | undefined {
  if (typeof raw !== 'object' || raw === null) return undefined;
  const record = raw as Readonly<Record<string, unknown>>;
  const name = asBoundedString(record['name'], MAX_HIERARCHY_NAME_LENGTH);
  const callId = asBoundedString(record['call_id'], MAX_HIERARCHY_CALL_ID_LENGTH);
  if (!name || !callId) return undefined;
  const siblingOrdinal = asFiniteNumber(record['sibling_ordinal']);
  return siblingOrdinal !== undefined
    ? { name, call_id: callId, sibling_ordinal: siblingOrdinal }
    : { name, call_id: callId };
}

/**
 * Safely normalizes `parent_agent_path` and legacy `parent_agent_name` /
 * `parent_agent_call_id` fields from event metadata, bounding depth and field
 * lengths to match `entities/message` contracts.
 */
export function normalizeTimelineHierarchy(metadata: TimelineEventMetadata | undefined): NormalizedHierarchy {
  const rawPath = metadata?.parent_agent_path;
  const path: TimelineAgentPathEntry[] = [];

  if (Array.isArray(rawPath) && rawPath.length <= MAX_HIERARCHY_DEPTH) {
    for (const entry of rawPath) {
      const parsed = parseAgentPathEntry(entry);
      if (parsed) path.push(parsed);
    }
  }

  const legacyName = asBoundedString(metadata?.parent_agent_name, MAX_HIERARCHY_NAME_LENGTH);
  const legacyCallId = asBoundedString(metadata?.parent_agent_call_id, MAX_HIERARCHY_CALL_ID_LENGTH);
  const legacyOwner = legacyName && legacyCallId ? { name: legacyName, call_id: legacyCallId } : undefined;

  const owner = path[0] ?? legacyOwner;
  const scope = path.at(-1) ?? legacyOwner;
  return { path, owner, scope };
}

function applicationGroupId(callId: string | undefined): string {
  return callId ? `application:${callId}` : '';
}

function resolveBranchOrMapGroupKey(metadata: TimelineEventMetadata | undefined): string | undefined {
  if (!metadata) return undefined;
  const parallelBranch =
    asBoundedString(metadata.parallel_branch_id, MAX_HIERARCHY_CALL_ID_LENGTH) ??
    asBoundedString(metadata.branch_id, MAX_HIERARCHY_CALL_ID_LENGTH);
  if (parallelBranch) return `parallel:${parallelBranch}`;

  const node = asBoundedString(metadata.langgraph_node, MAX_HIERARCHY_NAME_LENGTH);
  if (!node) return undefined;

  const mapItem = asFiniteNumber(metadata.map_item_index);
  if (mapItem !== undefined) return `map:${node}:${mapItem}`;

  const iteration = asFiniteNumber(metadata.iteration_index);
  if (iteration !== undefined) return `iteration:${node}:${iteration}`;

  return undefined;
}

function resolveGroupKey(
  metadata: TimelineEventMetadata | undefined,
  owner: TimelineAgentPathEntry | undefined,
  fallbackIndex: number,
): string {
  if (owner) return applicationGroupId(owner.call_id) || `row:${fallbackIndex}`;

  const structuredKey = resolveBranchOrMapGroupKey(metadata);
  if (structuredKey) return structuredKey;

  const namespace = asBoundedString(metadata?.langgraph_checkpoint_ns, MAX_HIERARCHY_NAME_LENGTH);
  return namespace ? `task:${namespace}` : `row:${fallbackIndex}`;
}

function resolveStepKind(
  eventType: string,
  metadata: TimelineEventMetadata | undefined,
  isNested: boolean,
): TimelineStepKind {
  if (eventType === PIPELINE_SOCKET_MESSAGE_TYPE.AgentHitlInterrupt) return 'interrupt';
  if (metadata?.parallel_branch_id || metadata?.branch_id) return 'parallel_child';
  if (asFiniteNumber(metadata?.map_item_index) !== undefined) return 'map_child';
  if (isNested) return 'nested_application';
  if (eventType === PIPELINE_SOCKET_MESSAGE_TYPE.AgentLlmStart) return 'llm';
  if (eventType === PIPELINE_SOCKET_MESSAGE_TYPE.AgentToolStart) return 'tool';
  return 'node';
}

function resolveToolId(event: TimelineRunSocketEvent, metadata: TimelineEventMetadata | undefined): string | undefined {
  const rawToolName = event.response_metadata.tool_name ?? '';
  const resolved =
    metadata?.toolkit_name ||
    event.response_metadata.toolkit_name ||
    toolkitNameFromRawToolName(rawToolName);
  if (!resolved) return undefined;
  if (resolved === 'pyodide_sandbox' || resolved === 'pyodide') {
    return metadata?.langgraph_node || resolved;
  }
  return resolved;
}

function resolveWrapperOwner(
  metadata: TimelineEventMetadata | undefined,
  isNested: boolean,
  fallbackName: string,
): TimelineAgentPathEntry | undefined {
  if (isNested || !metadata) return undefined;
  if (metadata.toolkit_type !== 'application' && metadata.toolkit_type !== 'pipeline') return undefined;
  const callId = asBoundedString(metadata.parent_agent_call_id, MAX_HIERARCHY_CALL_ID_LENGTH);
  if (!callId) return undefined;
  return {
    call_id: callId,
    name: metadata.original_name || metadata.display_name || fallbackName,
  };
}

function resolveGroupMeta(
  id: string,
  metadata: TimelineEventMetadata | undefined,
  hierarchy: NormalizedHierarchy,
  stepIndex: number,
): { readonly isNested: boolean; readonly groupId: string; readonly scopeId: string; readonly groupLabel: string } {
  const isNested = Boolean(hierarchy.owner);
  const wrapperOwner = resolveWrapperOwner(metadata, isNested, id);
  const effectiveOwner = hierarchy.owner ?? wrapperOwner;
  const effectiveScope = hierarchy.scope ?? wrapperOwner;
  const groupId = resolveGroupKey(metadata, effectiveOwner, stepIndex);
  const scopeId = applicationGroupId(effectiveScope?.call_id) || groupId;
  const groupLabel = effectiveOwner?.name || metadata?.langgraph_node || id;
  return { isNested, groupId, scopeId, groupLabel };
}

function buildStep(
  id: string,
  status: string,
  event: TimelineRunSocketEvent,
  metadata: TimelineEventMetadata | undefined,
  hierarchy: NormalizedHierarchy,
  stepIndex: number,
  createdAt: number,
): MutableGroupedStep {
  const { isNested, groupId, scopeId, groupLabel } = resolveGroupMeta(id, metadata, hierarchy, stepIndex);
  const state = event.response_metadata.state ? { ...event.response_metadata.state } : {};

  return {
    id,
    langgraph_node: metadata?.langgraph_node,
    langgraph_checkpoint_ns: metadata?.langgraph_checkpoint_ns,
    status,
    state,
    created_at: createdAt,
    tool_run_id: event.response_metadata.tool_run_id,
    group_id: groupId,
    scope_id: scopeId,
    group_label: groupLabel,
    is_nested: isNested,
    step_kind: resolveStepKind(event.type, metadata, isNested),
    iteration_index: asFiniteNumber(metadata?.iteration_index),
    sibling_ordinal: asFiniteNumber(metadata?.sibling_ordinal),
    ...(hierarchy.path.length > 0 ? { parent_agent_path: hierarchy.path } : {}),
  };
}

function findLastMatchingStep(
  steps: readonly MutableGroupedStep[],
  predicate: (step: MutableGroupedStep) => boolean,
): MutableGroupedStep | undefined {
  for (let index = steps.length - 1; index >= 0; index -= 1) {
    const candidate = steps[index];
    if (candidate && predicate(candidate)) return candidate;
  }
  return undefined;
}

function handleCompletionEvent(steps: readonly MutableGroupedStep[], event: TimelineRunSocketEvent): void {
  const { tool_run_id: toolRunId } = event.response_metadata;
  if (!toolRunId) return;

  const isError = event.type === PIPELINE_SOCKET_MESSAGE_TYPE.AgentToolError;
  const target = findLastMatchingStep(steps, step => step.tool_run_id === toolRunId);
  if (!target) return;

  target.status = isError ? PipelineStatus.Error : PipelineStatus.Completed;
  if (isError && typeof event.content === 'string') target.error = event.content;
}

function handleTransitionalEdge(
  steps: MutableGroupedStep[],
  event: TimelineRunSocketEvent,
  metadata: TimelineEventMetadata | undefined,
  hierarchy: NormalizedHierarchy,
  createdAt: number,
): void {
  const node = metadata?.langgraph_node;
  if (!node || node === 'StateDefaultNode') return;

  const scopeId = applicationGroupId(hierarchy.scope?.call_id);
  const namespace = metadata?.langgraph_checkpoint_ns;
  const stateCopy = event.response_metadata.state ? { ...event.response_metadata.state } : {};

  const target = findLastMatchingStep(steps, step => {
    if (hierarchy.owner) {
      return (
        Boolean(step.is_nested) &&
        step.scope_id === scopeId &&
        (namespace ? step.langgraph_checkpoint_ns === namespace : step.langgraph_node === node)
      );
    }
    return !step.is_nested && step.langgraph_node === node;
  });

  if (target) {
    target.state = stateCopy;
    if (target.status === PipelineStatus.InProgress) target.status = PipelineStatus.Completed;
    return;
  }

  const label = metadata?.original_name || node;
  steps.push(buildStep(label, PipelineStatus.Completed, event, metadata, hierarchy, steps.length, createdAt));
}

function resolveLlmStartStepId(
  event: TimelineRunSocketEvent,
  metadata: TimelineEventMetadata | undefined,
  isNested: boolean,
): string | undefined {
  if (isNested) return metadata?.langgraph_node || event.response_metadata.tool_name;
  return metadata?.original_name || metadata?.langgraph_node;
}

function resolveStartStepId(
  event: TimelineRunSocketEvent,
  metadata: TimelineEventMetadata | undefined,
  hierarchy: NormalizedHierarchy,
): string | undefined {
  if (event.type === PIPELINE_SOCKET_MESSAGE_TYPE.AgentLlmStart) {
    return resolveLlmStartStepId(event, metadata, Boolean(hierarchy.owner));
  }
  if (event.type === PIPELINE_SOCKET_MESSAGE_TYPE.AgentToolStart) {
    return hierarchy.owner ? event.response_metadata.tool_name : resolveToolId(event, metadata);
  }
  if (event.type === PIPELINE_SOCKET_MESSAGE_TYPE.AgentHitlInterrupt) {
    return event.response_metadata.node_name || metadata?.langgraph_node;
  }
  return undefined;
}

const COMPLETION_EVENT_TYPES: ReadonlySet<string> = new Set([
  PIPELINE_SOCKET_MESSAGE_TYPE.AgentLlmEnd,
  PIPELINE_SOCKET_MESSAGE_TYPE.AgentToolEnd,
  PIPELINE_SOCKET_MESSAGE_TYPE.AgentToolError,
]);

function applyRunEvent(steps: MutableGroupedStep[], event: TimelineRunSocketEvent, now: () => number): void {
  const metadata = event.response_metadata?.metadata;
  const hierarchy = normalizeTimelineHierarchy(metadata);

  if (COMPLETION_EVENT_TYPES.has(event.type)) {
    handleCompletionEvent(steps, event);
    return;
  }

  if (event.type === PIPELINE_SOCKET_MESSAGE_TYPE.AgentOnTransitionalEdge) {
    handleTransitionalEdge(steps, event, metadata, hierarchy, now());
    return;
  }

  if (event.type === PIPELINE_SOCKET_MESSAGE_TYPE.PipelineFinish) {
    const last = steps.at(-1);
    if (last && last.status === PipelineStatus.InProgress) last.status = PipelineStatus.Completed;
    return;
  }

  const startId = resolveStartStepId(event, metadata, hierarchy);
  if (startId) {
    steps.push(buildStep(startId, PipelineStatus.InProgress, event, metadata, hierarchy, steps.length, now()));
  }
}

/**
 * Pure reducer that converts an ordered stream of `TimelineRunSocketEvent`s into
 * grouped timeline steps (`GroupedTimelineStep[]`), preserving `node_errors`
 * on step state and grouping nested applications, Map/Parallel children, and
 * consecutive loop/task steps.
 */
export function normalizeRunEventsToGroupedTimeline(
  events: readonly TimelineRunSocketEvent[],
  now: () => number = () => Date.now(),
): readonly GroupedTimelineStep[] {
  const steps: MutableGroupedStep[] = [];
  for (const event of events) {
    applyRunEvent(steps, event, now);
  }
  return steps;
}
