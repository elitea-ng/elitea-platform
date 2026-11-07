/**
 * The Memory tab's filters and how they become the list route's query
 * (`listAgentMemory`): kind, status, the person an entry is about, who wrote
 * it (the agent or a person) and a created-at date range, plus a text search.
 *
 * Dates are the reader's own calendar days: "from" is the start of that day
 * and "to" the start of the day after, both in the browser's time zone, so a
 * range of one day covers that whole day where the reader is.
 */
import type { AgentMemoryKind, AgentMemoryStatus, ListAgentMemoryParams } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';

export const MEMORY_PAGE_SIZE = 50;
/** The route's own bound on one page; the export reads in pages of it. */
export const MEMORY_EXPORT_PAGE_SIZE = 200;

export type MemorySource = 'agent' | 'person';

export interface MemoryFilters {
  readonly query: string;
  readonly kind: AgentMemoryKind | '';
  readonly status: AgentMemoryStatus | '';
  /** The subject's user id as a string, or '' for anyone. */
  readonly personId: string;
  readonly source: MemorySource | '';
  /** `YYYY-MM-DD` from a date input, or ''. */
  readonly from: string;
  readonly to: string;
}

export const EMPTY_MEMORY_FILTERS: MemoryFilters = { query: '', kind: '', status: '', personId: '', source: '', from: '', to: '' };

const DAY = /^\d{4}-\d{2}-\d{2}$/;

/** The start of a calendar day in the reader's time zone, as RFC 3339; `undefined` for anything else. */
export function dayStart(day: string, addDays = 0): string | undefined {
  if (!DAY.test(day)) return undefined;
  const [year, month, date] = day.split('-').map(Number) as [number, number, number];
  const start = new Date(year, month - 1, date + addDays);
  return Number.isNaN(start.getTime()) ? undefined : start.toISOString();
}

/** The list route's query for one page of the filtered list. */
export function filtersToParams(filters: MemoryFilters, offset: number, limit: number = MEMORY_PAGE_SIZE): ListAgentMemoryParams {
  const query = filters.query.trim().slice(0, 256);
  const person = /^[1-9]\d{0,9}$/.test(filters.personId) ? Number(filters.personId) : undefined;
  const after = dayStart(filters.from);
  const before = dayStart(filters.to, 1);
  return {
    ...(query !== '' ? { query } : {}),
    ...(filters.kind !== '' ? { kind: filters.kind } : {}),
    ...(filters.status !== '' ? { status: filters.status } : {}),
    ...(person !== undefined ? { subject_user_id: person } : {}),
    ...(filters.source !== '' ? { source: filters.source } : {}),
    ...(after !== undefined ? { created_after: after } : {}),
    ...(before !== undefined ? { created_before: before } : {}),
    limit,
    ...(offset > 0 ? { offset } : {}),
  };
}

/**
 * The server's own sentence for a refusal, when it sent one (`{error, code}`
 * from `agentmemory/handler.go`'s writeError), else `fallback`. Rebuilt here
 * rather than imported: `no-sideways-features` keeps each feature's helpers
 * its own (`settings/lib/memory/longTermMemoryHelpers.ts` has the same one).
 */
export function memoryErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    const { body } = error.failure;
    if (typeof body === 'object' && body !== null) {
      const detail = (body as Record<string, unknown>)['error'];
      if (typeof detail === 'string' && detail !== '') return detail;
    }
    if (typeof body === 'string' && body !== '') return body;
  }
  return fallback;
}
