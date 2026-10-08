import { useInfiniteQuery, type InfiniteData } from '@tanstack/react-query';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import type { ConversationSummary } from '@/db/repo';
import { queryKeys } from '@/api/queries/keys';
import { syncManager } from '@/sync/appSync';
import { useDeviceCopy } from '@/api/queries/deviceCopy';
import { offlineConversationRows, readWithFallback } from '@/sync/offlineReads';

/** A list row as any supported server sends it (`is_pinned` is absent on a 1.3 server; see src/db/repo.ts). */
export type { ConversationSummary };

export const CONVERSATIONS_PAGE_SIZE = 30;

export interface ConversationPage {
  rows: ConversationSummary[];
  total: number;
  offset: number;
  /** Read from the encrypted offline store because the workspace did not answer. */
  offline?: boolean;
}

/**
 * One page of the caller's own conversations in a project, newest first.
 * When the workspace does not answer and the offline store is open, the page
 * comes from the store instead (`offline: true`).
 */
export async function fetchConversationPage(
  projectId: number,
  offset: number,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<ConversationPage> {
  const online = await readWithFallback<{ page: { rows?: ConversationSummary[]; total?: number } | undefined } | ConversationPage>(
    async (deadline) => ({
      page: await unwrap(
        client.GET('/elitea_core/conversations/prompt_lib/{project_id}', {
          params: {
            path: { project_id: String(projectId) },
            query: { mine: true, limit: CONVERSATIONS_PAGE_SIZE, offset },
          },
          signal: deadline,
        }),
      ),
    }),
    async () => {
      const stored = await offlineConversationRows(projectId, offset, CONVERSATIONS_PAGE_SIZE);
      return stored === null ? null : { ...stored, offset, offline: true };
    },
    { signal },
  );
  if (!('page' in online)) return online;
  const page = online.page;
  // Online again: bring the offline copy up to date in the background.
  if (offset === 0) syncManager.requestSync(projectId);
  const rows = Array.isArray(page?.rows) ? page.rows : [];
  const total = typeof page?.total === 'number' ? page.total : rows.length;
  return { rows, total, offset };
}

/** Offset of the next page, or undefined when the listing is exhausted. */
export function nextConversationOffset(last: ConversationPage): number | undefined {
  const next = last.offset + last.rows.length;
  if (last.rows.length === 0 || next >= last.total) return undefined;
  return next;
}

/**
 * Flattens pages, dropping rows a later page repeats (a conversation created
 * while paging shifts the offsets by one).
 */
export function flattenConversationPages(pages: readonly ConversationPage[] | undefined): ConversationSummary[] {
  const seen = new Set<number>();
  const out: ConversationSummary[] = [];
  for (const page of pages ?? []) {
    for (const row of page.rows) {
      if (seen.has(row.id)) continue;
      seen.add(row.id);
      out.push(row);
    }
  }
  return out;
}

/**
 * The saved first page as react-query placeholder data (DEF-R5): an offline
 * cold start shows the list at once while the workspace is asked (stale while
 * revalidate). It is not flagged `offline`; the notice appears only when the
 * workspace then fails to answer.
 */
export function savedFirstPage(
  saved: { rows: ConversationSummary[]; total: number } | null,
): InfiniteData<ConversationPage, number> | undefined {
  if (saved === null || saved.rows.length === 0) return undefined;
  return { pages: [{ rows: saved.rows, total: saved.total, offset: 0 }], pageParams: [0] };
}

export function useConversations(projectId: number | null) {
  const saved = useDeviceCopy(queryKeys.conversations(projectId ?? 0), projectId !== null, () =>
    offlineConversationRows(projectId as number, 0, CONVERSATIONS_PAGE_SIZE),
  );
  return useInfiniteQuery({
    queryKey: queryKeys.conversations(projectId ?? 0),
    enabled: projectId !== null,
    placeholderData: savedFirstPage(saved),
    initialPageParam: 0,
    queryFn: ({ pageParam, signal }) =>
      fetchConversationPage(projectId as number, pageParam, sessionApiClient(), signal),
    getNextPageParam: nextConversationOffset,
  });
}
