import { useQuery } from '@tanstack/react-query';

import { sessionApiClient, type ApiClient } from '@/api/client';
import type { components } from '@/api/generated/elitea.gen';
import { queryKeys } from '@/api/queries/keys';
import { PAUSE_FINISH_REASONS, capText, displayValue, waitOf, type ToolStep } from '@/features/chat/toolActivity';

/**
 * Tool history of a reloaded conversation (client contract 1.1):
 * listMessageTraces gives the light steps a resting chip draws, and
 * getMessageTrace one step's inputs and output, fetched only when the user
 * opens it. Nothing here is written to the offline store; the query cache
 * keeps it in memory and is wiped with the session.
 */

type TraceStep = components['schemas']['MessageTraceStep'];

/** At most this many message groups per list call (the server's limit). */
export const MAX_TRACE_GROUPS = 200;
export const TRACE_PAGE = 500;
/** A bound on the pages read for one list (TRACE_PAGE × this many tool calls). */
const MAX_TRACE_PAGES = 20;

const asRecord = (v: unknown): Record<string, unknown> | null =>
  v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
const asName = (v: unknown): string | null => (typeof v === 'string' && v.trim() !== '' ? v.trim().slice(0, 200) : null);
const time = (v: unknown): number | null => {
  if (typeof v !== 'string') return null;
  const ms = Date.parse(v);
  return Number.isFinite(ms) ? ms : null;
};

/** A persisted step as the chip and the detail sheet read it. */
export interface PersistedToolStep extends ToolStep {
  /** The trace step id, for getMessageTrace. */
  traceId: number;
  messageGroupId: number;
}

export function traceToStep(row: TraceStep): PersistedToolStep | null {
  if (!Number.isSafeInteger(row?.id) || !Number.isSafeInteger(row?.message_group_id)) return null;
  const attrs = asRecord(row.attrs) ?? {};
  const meta = asRecord(attrs.metadata) ?? {};
  const toolMeta = asRecord(attrs.tool_meta) ?? {};
  const chunks = asRecord(attrs.tool_output_chunks);
  const endedAt = time(row.finished_at);
  // Contract 1.2: a call that paused for the user is stored with a pause
  // reason and is not an error. An unknown reason is read by `is_error`.
  const pause = asRecord(attrs.pause);
  const paused = !row.is_error && ((typeof row.finish_reason === 'string' && PAUSE_FINISH_REASONS.has(row.finish_reason)) || pause !== null);
  return {
    id: `trace-${row.id}`,
    traceId: row.id,
    messageGroupId: row.message_group_id,
    name: asName(row.tool_name) ?? asName(toolMeta.name) ?? 'tool',
    displayName: asName(meta.display_name) ?? asName(toolMeta.display_name),
    toolkit: asName(meta.toolkit_name),
    parentAgent: asName(row.parent_agent_name) ?? asName(meta.parent_agent_name),
    status: row.is_error ? 'error' : paused ? 'waiting' : endedAt !== null ? 'done' : 'running',
    ...(paused
      ? {
          waitingFor: waitOf(row.finish_reason, pause?.guardrail_type),
          pauseId: typeof pause?.interrupt_id === 'string' && pause.interrupt_id !== '' ? pause.interrupt_id : null,
        }
      : {}),
    startedAt: time(row.started_at),
    endedAt,
    inputs: null,
    inputsTruncated: false,
    output: null,
    outputTruncated: false,
    // A chunked output whose reassembly did not complete.
    partial: chunks !== null && chunks.complete === false,
    error: null,
    chunks: null,
  };
}

/** The available-or-not outcome of a 1.1 read against any server. */
function unavailable(status: number): boolean {
  return status === 404 || status === 403 || status === 501;
}

/**
 * The tool calls of the given message groups, grouped by group id, in start
 * order. Null when the server does not offer traces (an older server, or no
 * permission): the thread then shows no tool row for persisted answers.
 */
export async function fetchMessageTraces(
  projectId: number,
  conversationId: number,
  groupIds: readonly number[],
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<Map<number, PersistedToolStep[]> | null> {
  const ids = [...new Set(groupIds.filter((id) => Number.isSafeInteger(id) && id > 0))].slice(-MAX_TRACE_GROUPS);
  const out = new Map<number, PersistedToolStep[]>();
  if (ids.length === 0) return out;
  // Rows come oldest first, so a single page would drop the NEWEST answers'
  // calls: read pages until a short one.
  for (let page = 0; page < MAX_TRACE_PAGES; page++) {
    const { data, response } = await client.GET('/elitea_core/message_traces/prompt_lib/{project_id}/{conversation_id}', {
      params: {
        path: { project_id: String(projectId), conversation_id: conversationId },
        query: { kind: 'tool_call', message_group_ids: ids.join(','), limit: TRACE_PAGE, offset: page * TRACE_PAGE },
      },
      signal,
    });
    if (!response.ok) {
      if (unavailable(response.status)) return page === 0 ? null : out;
      throw new Error(`message traces: HTTP ${response.status}`);
    }
    const rows = Array.isArray(data?.rows) ? data.rows : [];
    for (const row of rows) {
      if (row?.kind !== 'tool_call') continue;
      const step = traceToStep(row);
      if (!step) continue;
      const list = out.get(step.messageGroupId) ?? [];
      list.push(step);
      out.set(step.messageGroupId, list);
    }
    if (rows.length < TRACE_PAGE) break;
  }
  for (const list of out.values()) list.sort((a, b) => (a.startedAt ?? 0) - (b.startedAt ?? 0) || a.traceId - b.traceId);
  return out;
}

export interface TraceDetail {
  inputs: string | null;
  inputsTruncated: boolean;
  output: string | null;
  outputTruncated: boolean;
}

/** One step's inputs and output, capped like a live step's. */
export async function fetchMessageTraceDetail(
  projectId: number,
  stepId: number,
  messageGroupId: number,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<TraceDetail | null> {
  const { data, response } = await client.GET('/elitea_core/message_trace/prompt_lib/{project_id}/{step_id}', {
    params: { path: { project_id: String(projectId), step_id: stepId }, query: { message_group_id: messageGroupId } },
    signal,
  });
  if (!response.ok) {
    if (unavailable(response.status)) return null;
    throw new Error(`message trace: HTTP ${response.status}`);
  }
  const inputs = displayValue(data?.tool_inputs);
  const output = displayValue(data?.tool_output);
  const cappedIn = inputs === null ? null : capText(inputs);
  const cappedOut = output === null ? null : capText(output);
  return {
    inputs: cappedIn?.text ?? null,
    inputsTruncated: cappedIn?.truncated ?? false,
    output: cappedOut?.text ?? null,
    outputTruncated: cappedOut?.truncated ?? false,
  };
}

export function useMessageTraces(
  projectId: number | null,
  conversationId: number | null,
  groupIds: readonly number[],
  enabled: boolean,
) {
  const key = groupIds.join(',');
  return useQuery({
    queryKey: [...queryKeys.traces(projectId ?? 0), conversationId ?? 0, key],
    enabled: enabled && projectId !== null && conversationId !== null && groupIds.length > 0,
    staleTime: 60_000,
    retry: false,
    queryFn: ({ signal }) => fetchMessageTraces(projectId as number, conversationId as number, groupIds, sessionApiClient(), signal),
  });
}

export function useMessageTraceDetail(projectId: number | null, step: Pick<PersistedToolStep, 'traceId' | 'messageGroupId'> | null) {
  return useQuery({
    queryKey: [...queryKeys.projectRoot(projectId ?? 0), 'trace', step?.traceId ?? 0],
    enabled: projectId !== null && step !== null,
    // Tool I/O is kept no longer than it is looked at.
    gcTime: 30_000,
    retry: false,
    queryFn: ({ signal }) =>
      fetchMessageTraceDetail(projectId as number, step!.traceId, step!.messageGroupId, sessionApiClient(), signal),
  });
}
