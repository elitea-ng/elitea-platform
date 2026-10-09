import { useCallback, useMemo } from 'react';

import { keepPreviousData, useInfiniteQuery } from '@tanstack/react-query';

import type { Toolkit } from '@/entities/toolkit';
import { getListToolkitInstancesQueryKey, listToolkitInstances } from '@/shared/api/generated/toolkits/toolkits';
import { unwrapListPage } from '@/shared/api/unwrap';

/**
 * One page of `listToolkitInstances` is 20 rows — the page size EliteaUI's
 * picker (`useDropdownData.jsx`) uses. Paging is OFFSET-based (`offset = page *
 * INSTANCE_PAGE_SIZE`), so `limit` stays a constant 20 and never trips the
 * handler's `limit > 100 → reset to 20` clamp
 * (`internal/api/v2/toolkits/handler.go`, `maxInstanceListLimit`).
 */
export const INSTANCE_PAGE_SIZE = 20;

/** What one picker section asks the server for; both fields are server-side filters. */
export interface ToolkitInstanceFilter {
  /** `true` pages MCP servers only, `false` everything else (the server also drops `application` rows for either value). */
  readonly mcp: boolean;
  /** Case-insensitive name/description substring; trimmed here, omitted from the request when empty. */
  readonly query: string;
}

/**
 * A cursor over ONE server-filtered view of the project's toolkit instances.
 *
 * The Toolkit and MCP sections of the picker each own one, so each pages its
 * own rows (`mcp=false` / `mcp=true`) the way EliteaUI's picker does, and a
 * section can no longer be starved by the other type sorting ahead of it. The
 * name search is server-side too (`query`), so a match is found wherever it
 * sorts. `keepPreviousData` keeps the old rows on screen while a new search
 * loads, instead of flashing the list empty.
 */
export interface ToolkitInstancePager {
  readonly rows: readonly Toolkit[];
  readonly isFetching: boolean;
  readonly hasMore: boolean;
  /** Fetches the next page; a no-op while a fetch is in flight or the list is exhausted. */
  readonly fetchMore: () => void;
}

function toToolkitInstance(row: unknown): Toolkit {
  return row as Toolkit;
}

export function useToolkitInstancePager(projectId: string | undefined, { mcp, query: search }: ToolkitInstanceFilter): ToolkitInstancePager {
  const query = search.trim();
  const result = useInfiniteQuery({
    queryKey: [...getListToolkitInstancesQueryKey(projectId ?? ''), 'pager', { mcp, query }] as const,
    enabled: projectId !== undefined,
    initialPageParam: 0,
    placeholderData: keepPreviousData,
    queryFn: async ({ pageParam }) =>
      unwrapListPage<unknown>(
        await listToolkitInstances(projectId ?? '', {
          limit: INSTANCE_PAGE_SIZE,
          offset: pageParam * INSTANCE_PAGE_SIZE,
          mcp,
          ...(query !== '' ? { query } : {}),
        }),
        'listToolkitInstances',
      ),
    getNextPageParam: (lastPage, allPages) => {
      // A short/empty page means the server has nothing more to give, even if
      // its reported `total` disagrees — stop rather than re-request the same
      // exhausted offset forever.
      if (lastPage.rows.length === 0) return undefined;
      const fetched = allPages.reduce((sum, page) => sum + page.rows.length, 0);
      return fetched < lastPage.total ? allPages.length : undefined;
    },
  });

  // useMemo, not a bare expression: this flattened array is a prop/dep
  // downstream, and `flatMap`/`map` return FRESH arrays each render.
  const rows = useMemo(() => (result.data?.pages ?? []).flatMap((page) => page.rows).map(toToolkitInstance), [result.data]);
  const { fetchNextPage, isFetching, hasNextPage } = result;
  // The guard matters: `fetchNextPage` cancels an in-flight fetch by default, and
  // the scroll trigger fires on every scroll event near the end of the list.
  const fetchMore = useCallback(() => {
    if (!isFetching && hasNextPage) void fetchNextPage();
  }, [fetchNextPage, isFetching, hasNextPage]);

  return { rows, isFetching, hasMore: hasNextPage, fetchMore };
}
