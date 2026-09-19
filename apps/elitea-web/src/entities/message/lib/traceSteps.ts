/**
 * The persisted trace-step rows, turned back into the `meta.tool_calls` shape
 * the message normaliser already knows how to render.
 *
 * ── WHY THIS EXISTS ────────────────────────────────────────────────────────
 *
 * Tool-call detail used to live on `chat_message_group.meta`, and
 * `normaliseAssistantMessage` still builds a turn's `toolActions[]` from
 * `meta.thinking_steps` / `meta.tool_calls` (`buildToolActions`). The
 * trace-step migration moved that detail into its own
 * `<tenant>.chat_message_trace_step` table — `agent_trace.go` writes it, and
 * the conversation read deliberately stops carrying it (asserted by
 * `chat.tail-trace-steps.spec.ts`'s "conversation details carry no tool_calls
 * or thinking_steps"). So a reloaded conversation had an EMPTY `toolActions`
 * for every turn, `ApplicationAnswerThinking` returned `null` on
 * `!actions.length`, and the "Thought for …" panel that holds the pins was not
 * rendered at all — #951.
 *
 * `GET /elitea_core/message_traces/…` serves the rows. This module maps them
 * back onto `ToolCallStepWire` so the existing builder — with all of its
 * toolkit-name, toolkit-type, icon and sub-agent-collapsing rules — does the
 * rest, rather than a second renderer growing its own copy of them. The map is
 * Go's own `reconstructCurrentAgentTrace` (`agent_trace.go`), which rebuilds
 * the same wire entry from the same columns.
 *
 * ── THE HEAVY FIELDS ARE FETCHED ON OPEN, NOT LISTED ───────────────────────
 *
 * The listing is deliberately light: `tool_inputs`, `tool_output`, `text` and
 * `thinking` are TOASTed and are served one row at a time by
 * `GET /elitea_core/message_trace/…` instead. So every restored step carries
 * its ROW IDENTITY (`trace_step_id` / `trace_message_group_id`) and `ToolModal`
 * fetches that one row's body when the reader opens the pin
 * (`model/traceStepDetail.tsx`) — the same two-call split `RunHistoryTrace`
 * already uses over these two routes.
 *
 * That identity is also what lets a THINKING step survive: `buildToolActions`
 * drops a thinking step with no `text` (a transition marker), and the listing
 * never carries text — so a restored one would vanish. `buildLlmToolAction`
 * keeps it when the row identity is present, because the listing has already
 * excluded the blank ones (`has_visible_content`): a listed thinking row is
 * one that HAS something to show, just not in this projection.
 */
import type { ThinkingStepWire, ToolCallStepWire } from './wire';

/**
 * One row of the light `message_traces` listing, read structurally rather than
 * against the generated zod type: this module is in `entities/`, and the
 * generated model lives behind the shared API layer.
 */
export interface TraceStepRow {
  readonly id: number;
  readonly message_group_id: number;
  readonly kind: string;
  readonly tool_name?: string | null;
  readonly parent_agent_name?: string | null;
  readonly parent_agent_call_id?: string | null;
  readonly started_at?: string | null;
  readonly finished_at?: string | null;
  readonly is_error?: boolean;
  readonly attrs?: Record<string, unknown> | null;
}

function record(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}

function text(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined;
}

/**
 * One `tool_call` row as the wire entry `buildToolActions` consumes.
 *
 * `metadata` and `tool_meta` come straight out of the bounded `attrs` sidecar
 * the writer put them in, with the row's own hierarchy columns layered on top
 * — the same precedence `reconstructCurrentAgentTrace` applies.
 */
function toToolCallStep(row: TraceStepRow): ToolCallStepWire {
  const attrs = record(row.attrs) ?? {};
  const metadata: Record<string, unknown> = { ...record(attrs['metadata']) };
  const parentAgentName = text(row.parent_agent_name);
  const parentAgentCallId = text(row.parent_agent_call_id);
  if (parentAgentName !== undefined) metadata['parent_agent_name'] = parentAgentName;
  if (parentAgentCallId !== undefined) metadata['parent_agent_call_id'] = parentAgentCallId;
  const toolMeta = record(attrs['tool_meta']);
  return {
    trace_step_id: row.id,
    trace_message_group_id: row.message_group_id,
    tool_run_id: `trace-${String(row.id)}`,
    tool_name: text(row.tool_name) ?? 'Tool Call',
    metadata,
    ...(toolMeta === undefined ? {} : { tool_meta: toolMeta }),
    ...(row.started_at ? { timestamp_start: row.started_at } : {}),
    ...(row.finished_at ? { timestamp_finish: row.finished_at } : {}),
    // The listing does not carry the output, so an errored step is marked
    // without inventing a body for it — `isError` is what the row draws.
    ...(row.is_error === true ? { error: 'error' } : {}),
  };
}

/** One `thinking_step` row as the wire entry `buildToolActions` consumes. */
function toThinkingStep(row: TraceStepRow): ThinkingStepWire {
  return {
    trace_step_id: row.id,
    trace_message_group_id: row.message_group_id,
    ...(text(row.parent_agent_name) === undefined ? {} : { parent_agent_name: row.parent_agent_name as string }),
    ...(row.started_at ? { timestamp_start: row.started_at } : {}),
    ...(row.finished_at ? { timestamp_finish: row.finished_at } : {}),
    message: { id: `trace-${String(row.id)}`, response_metadata: {} },
  };
}

/** Both step kinds of one message group, as `resolveAssistantToolInputs` takes them. */
export interface PersistedTraceSteps {
  readonly toolCalls: readonly ToolCallStepWire[];
  readonly thinkingSteps: readonly ThinkingStepWire[];
}

/**
 * Group a conversation's trace rows by their message group, keyed by the
 * group id as a STRING: `MessageGroupWire.id` is `string | number` on the wire
 * (both spellings are produced — see its own doc), so the key is normalised
 * once here rather than at every lookup.
 */
export function groupTraceStepsByMessageGroup(
  rows: readonly TraceStepRow[],
): ReadonlyMap<string, PersistedTraceSteps> {
  const grouped = new Map<string, { toolCalls: ToolCallStepWire[]; thinkingSteps: ThinkingStepWire[] }>();
  for (const row of rows) {
    if (row.kind !== 'tool_call' && row.kind !== 'thinking_step') continue;
    const key = String(row.message_group_id);
    let steps = grouped.get(key);
    if (steps === undefined) {
      steps = { toolCalls: [], thinkingSteps: [] };
      grouped.set(key, steps);
    }
    if (row.kind === 'tool_call') steps.toolCalls.push(toToolCallStep(row));
    else steps.thinkingSteps.push(toThinkingStep(row));
  }
  return grouped;
}
