/**
 * The threads a schedule or a trigger can target: the caller's Local work
 * threads in this project that are in the cloud now.
 *
 * There is no list of handoffs. `listConversations` with `source=local_work`
 * and `mine=true` lists the caller's Local work threads (Main refuses a thread
 * whose user participant the caller is not), and each row's
 * `workspace_executor` (client contract 1.8) says who runs its turns now:
 * `cloud` is a handed-off thread. A thread that was handed off once and taken
 * back is `local`: Main still accepts it, but it is not offered here, because
 * every occurrence on it would record `skipped_thread_busy`.
 *
 * The listing has pages of at most 100 rows; this reads page after page until
 * the list ends, and stops at `MAX_ROWS` (the picker then says so).
 */
import { useMemo } from 'react';

import { useQuery } from '@tanstack/react-query';

import { getListConversationsQueryKey, listConversations } from '@/shared/api/generated/chat/chat';
import type { ConversationSummary } from '@/shared/api/generated/model';
import { unwrapListPage } from '@/shared/api/unwrap';

/** A thread the picker offers. */
interface HandedOffThread {
  readonly uuid: string;
  readonly name: string;
}

/** The most rows one page returns (v2.yaml `limit.maximum`). */
const PAGE = 100;
/** The most rows the picker reads: 10 pages. */
const MAX_ROWS = 1000;

const PARAMS = { source: 'local_work', mine: true } as const;

interface Listing {
  readonly rows: readonly ConversationSummary[];
  /** True when the list has more rows than `MAX_ROWS`. */
  readonly capped: boolean;
}

async function readAll(projectId: string, signal: AbortSignal): Promise<Listing> {
  const rows: ConversationSummary[] = [];
  for (let offset = 0; offset < MAX_ROWS; offset += PAGE) {
    const page = unwrapListPage<ConversationSummary>(await listConversations(projectId, { ...PARAMS, limit: PAGE, offset }, { signal }), 'threadTarget.threads');
    rows.push(...page.rows);
    if (page.rows.length < PAGE || rows.length >= page.total) return { rows, capped: false };
  }
  return { rows, capped: true };
}

export interface HandedOffThreads {
  readonly threads: readonly HandedOffThread[];
  /** Every Local work thread of the caller, by uuid: a stored target that is not in the cloud now still has a name. */
  readonly names: ReadonlyMap<string, string>;
  readonly isLoading: boolean;
  /** The caller has more than 1000 Local work threads; the picker shows the first 1000. */
  readonly capped: boolean;
}

export function useHandedOffThreads(projectId: string | undefined): HandedOffThreads {
  const project = projectId ?? '';
  const query = useQuery({
    queryKey: [...getListConversationsQueryKey(project, PARAMS), 'all-pages'],
    queryFn: ({ signal }) => readAll(project, signal),
    enabled: project !== '',
  });
  return useMemo(() => {
    const names = new Map<string, string>();
    const threads: HandedOffThread[] = [];
    for (const row of query.data?.rows ?? []) {
      if (typeof row.uuid !== 'string' || row.uuid === '') continue;
      names.set(row.uuid, row.name);
      if (row.workspace_executor === 'cloud') threads.push({ uuid: row.uuid, name: row.name });
    }
    return { threads, names, isLoading: query.isLoading, capped: query.data?.capped === true };
  }, [query.data, query.isLoading]);
}
