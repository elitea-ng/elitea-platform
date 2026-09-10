import { eliteaFetch } from '@/shared/api/generated/mutator';
import type { MessageTraceStep } from '@/shared/api/generated/model';
import { unwrapList } from '@/shared/api/unwrap';

function record(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null ? value as Record<string, unknown> : undefined;
}

function messageRows(payload: unknown): readonly unknown[] | undefined {
  try { return unwrapList<unknown>(payload, 'conversation.messageList'); }
  catch { return undefined; } // Keep invalid-envelope handling at the existing caller boundary.
}

async function fetchTracePage(projectId: string | number, conversationId: string | number, groupIds: readonly number[], offset: number, signal?: AbortSignal): Promise<MessageTraceStep[]> {
  const query = new URLSearchParams({ message_group_ids: groupIds.join(','), limit: '500', offset: String(offset) });
  const response = await eliteaFetch<{ data: { rows: MessageTraceStep[] } }>(
    `/elitea_core/message_traces/prompt_lib/${String(projectId)}/${String(conversationId)}?${query.toString()}`,
    signal ? { signal } : {},
  );
  return response.data.rows;
}

/** Read light summaries for this message page. Heavy detail stays behind the step endpoint. */
export async function attachMessageTraces(payload: unknown, projectId: string | number, conversationId: string | number, signal?: AbortSignal): Promise<unknown> {
  const rows = messageRows(payload);
  if (!rows) return payload;
  const ids = [...new Set(rows.map(row => Number(record(row)?.['id'])).filter(id => Number.isSafeInteger(id) && id > 0))];
  if (ids.length === 0) return payload;
  const byGroup = new Map<number, MessageTraceStep[]>();
  let failed = false;
  try {
    for (let start = 0; start < ids.length; start += 200) {
      const groupIds = ids.slice(start, start + 200);
      for (let offset = 0; ; offset += 500) {
        const steps = await fetchTracePage(projectId, conversationId, groupIds, offset, signal);
        for (const step of steps) {
          if (!groupIds.includes(step.message_group_id)) continue;
          const group = byGroup.get(step.message_group_id) ?? [];
          group.push(step);
          byGroup.set(step.message_group_id, group);
        }
        if (steps.length < 500) break;
      }
    }
  } catch (error) {
    if (signal?.aborted) throw error;
    failed = true;
  }
  return enrichPayload(payload, rows, byGroup, String(projectId), String(conversationId), failed);
}

function enrichPayload(payload: unknown, rows: readonly unknown[], byGroup: ReadonlyMap<number, MessageTraceStep[]>, projectId: string, conversationId: string, failed: boolean): unknown {
  const enriched = rows.map(row => {
    const value = record(row);
    const id = Number(value?.['id']);
    const steps = byGroup.get(id) ?? [];
    if (!value || (!failed && steps.length === 0)) return row;
    return { ...value, persisted_trace: { projectId: String(projectId), conversationId: String(conversationId), messageGroupId: id, steps, failed } };
  });
  if (Array.isArray(payload)) return enriched;
  const envelope = record(payload) ?? {};
  return { ...envelope, [Array.isArray(envelope['items']) ? 'items' : 'rows']: enriched };
}
