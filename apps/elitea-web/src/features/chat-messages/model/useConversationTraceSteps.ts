/**
 * The conversation's persisted trace steps, in the shape the transcript
 * converter takes (#951).
 *
 * `chat.tail-trace-steps.spec.ts` measured the gap this closes: a settled
 * tool-calling turn renders its "Thought for …" panel and one
 * `chat-tool-action` row per step while it is on screen, and after a reload —
 * or after reopening the conversation — the panel is not rendered at all,
 * because the pins were built from `meta.tool_calls` and the trace-step
 * migration moved that detail into `chat_message_trace_step`. The rows
 * themselves never went anywhere: `/elitea_core/message_traces/…` answers the
 * same steps before and after. This hook is the reader nothing had.
 *
 * It is deliberately ONE listing per conversation rather than one per message:
 * the route is conversation scoped, the projection is light by design (the
 * heavy `tool_inputs`/`tool_output`/`text` columns are served one step at a
 * time by `/elitea_core/message_trace/…`), and a per-message read would put a
 * request behind every row of a long transcript.
 */
import { useMemo } from 'react';

import { useListMessageTraces } from '@/shared/api/generated/chat/chat';

import type { PersistedTraceSteps, TraceStepRow } from '@/entities/message/lib/traceSteps';
import { groupTraceStepsByMessageGroup } from '@/entities/message/lib/traceSteps';
/** An empty map, stable across renders so a conversation with no steps does not re-seed the transcript. */
const NO_TRACE_STEPS: ReadonlyMap<string, PersistedTraceSteps> = new Map();

export function useConversationTraceSteps(
  projectId: string | number | undefined,
  conversationId: string | number | undefined,
): ReadonlyMap<string, PersistedTraceSteps> {
  const project = projectId === undefined ? '' : String(projectId);
  // The route takes the NUMERIC conversation id; a chat addressed only by its
  // uuid has no listing to ask for and is left alone rather than asked for
  // `NaN`.
  const conversation = Number(conversationId);
  const enabled = project !== '' && Number.isInteger(conversation) && conversation > 0;

  const query = useListMessageTraces(
    project,
    enabled ? conversation : 0,
    { kind: 'tool_call' },
    { query: { enabled } },
  );

  // `.data.data`'s declared type includes the error-envelope variant, which
  // `eliteaFetch` throws rather than resolving with — the same cast
  // `RunHistoryTrace` establishes for this very endpoint.
  const rows = (query.data?.data as { readonly rows?: readonly TraceStepRow[] } | undefined)?.rows;

  return useMemo(() => (rows === undefined ? NO_TRACE_STEPS : groupTraceStepsByMessageGroup(rows)), [rows]);
}
