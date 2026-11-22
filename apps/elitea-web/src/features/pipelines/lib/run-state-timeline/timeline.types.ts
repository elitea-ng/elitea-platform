/**
 * Typed contracts for the grouped RunStateDialog timeline (`EL-3734` / `#1585`).
 *
 * Structurally compatible with both `RunTimelineEntry`
 * (`lib/flow-editor/helpers/parseRunsByEvent.support.ts`) and `RunStateDialog`'s
 * `RunStateStep` (`ui/state/runStateValues.ts`), adding grouping, scoping,
 * hierarchy, and structured continued-failure (`node_errors`) metadata.
 */

export interface TimelineAgentPathEntry {
  readonly name: string;
  readonly call_id: string;
  readonly sibling_ordinal?: number | undefined;
}

export type TimelineStepKind =
  | 'node'
  | 'llm'
  | 'tool'
  | 'interrupt'
  | 'map_child'
  | 'parallel_child'
  | 'nested_application';

export type ContinuedFailureHandling = 'continue' | 'error_output';

/**
 * Structured representation of a settled `node_errors` entry (5b-1 D12).
 * Rendered exclusively via React text nodes — never interpolated into raw HTML.
 */
export interface TimelineContinuedFailure {
  readonly node: string;
  readonly handled: ContinuedFailureHandling;
  readonly inputStep?: number | undefined;
  readonly supportRef?: string | undefined;
  readonly code?: string | undefined;
  readonly message?: string | undefined;
}

export interface GroupedTimelineStep {
  readonly id: string;
  readonly langgraph_node?: string | undefined;
  readonly langgraph_checkpoint_ns?: string | undefined;
  readonly status?: string | undefined;
  readonly state?: Readonly<Record<string, unknown>> | undefined;
  readonly created_at: number | string;
  readonly tool_run_id?: string | undefined;
  readonly source?: string | undefined;
  readonly target?: string | undefined;
  readonly error?: string | undefined;
  readonly group_id?: string | undefined;
  readonly scope_id?: string | undefined;
  readonly group_label?: string | undefined;
  readonly is_nested?: boolean | undefined;
  readonly step_kind?: TimelineStepKind | undefined;
  readonly iteration_index?: number | undefined;
  readonly sibling_ordinal?: number | undefined;
  readonly parent_agent_path?: readonly TimelineAgentPathEntry[] | undefined;
}

export interface TimelineStepGroup {
  readonly groupId: string | undefined;
  readonly groupLabel: string;
  readonly start: number;
  readonly end: number;
  readonly stepCount: number;
  readonly hasContinuedFailure: boolean;
  readonly hasError: boolean;
}

export interface VisibleTimelineRow {
  readonly index: number;
  readonly end: number;
  readonly expanded: boolean;
}

export interface TimelineGroupState {
  readonly expandAll: boolean;
  readonly hasGroups: boolean;
  readonly groups: readonly TimelineStepGroup[];
  readonly visibleSteps: readonly VisibleTimelineRow[];
  readonly toggleGroup: (start: number, end: number, expanded: boolean) => void;
  readonly toggleExpandAll: (checked: boolean) => void;
}

export interface TimelineEventMetadata {
  readonly original_name?: string | undefined;
  readonly display_name?: string | undefined;
  readonly langgraph_node?: string | undefined;
  readonly langgraph_checkpoint_ns?: string | undefined;
  readonly toolkit_name?: string | undefined;
  readonly toolkit_type?: string | undefined;
  readonly parent_agent_name?: string | undefined;
  readonly parent_agent_call_id?: string | undefined;
  readonly parent_agent_path?: unknown;
  readonly sibling_ordinal?: number | undefined;
  readonly iteration_index?: number | undefined;
  readonly map_item_index?: number | undefined;
  readonly parallel_branch_id?: string | undefined;
  readonly branch_id?: string | undefined;
  readonly [key: string]: unknown;
}

export interface TimelineRunResponseMetadata {
  readonly metadata?: TimelineEventMetadata | undefined;
  readonly tool_run_id?: string | undefined;
  readonly tool_name?: string | undefined;
  readonly toolkit_name?: string | undefined;
  readonly node_name?: string | undefined;
  readonly next_step?: string | undefined;
  readonly state?: Readonly<Record<string, unknown>> | undefined;
  readonly [key: string]: unknown;
}

export interface TimelineRunSocketEvent {
  readonly type: string;
  readonly response_metadata: TimelineRunResponseMetadata;
  readonly content?: unknown;
}
