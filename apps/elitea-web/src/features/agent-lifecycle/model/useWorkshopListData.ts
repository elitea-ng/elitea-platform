import { useMemo } from 'react';

import { useInfiniteQuery } from '@tanstack/react-query';
import { useSearch } from '@tanstack/react-router';

import { PINNED_GROUP_ID } from '@/entities/entity-folder';
import { getListApplicationsQueryKey, listApplications } from '@/shared/api/generated/applications/applications';
import type { Application, ApplicationList, ListApplicationsParams } from '@/shared/api/generated/model';

/** Rows per server page; "load more" fetches the next one. */
const PAGE_SIZE = 40;

/** The Workshop list's own search params (ADR-0051): the status and origin facets and the sort. */
interface WorkshopSearch {
  readonly sort_by?: string;
  readonly sort_order?: string;
  readonly statuses?: readonly string[];
  readonly origin?: string;
}

export interface WorkshopListInput {
  readonly projectId: string | undefined;
  readonly agentsType: 'classic' | 'pipeline';
  /** The list is on screen: false on a mode that lists something else (Teams). */
  readonly enabled: boolean;
  readonly query: string;
  readonly tags: readonly string[];
  readonly folderId: string | undefined;
}

export interface WorkshopListData {
  /** Every row loaded so far, in the server's order. */
  readonly rows: readonly Application[];
  /** The first page: its `facets` and `total` describe the whole filtered list. */
  readonly wire: ApplicationList | undefined;
  readonly isLoading: boolean;
  readonly isError: boolean;
  /** A search or a filter is on, so an empty list means "nothing found" and not "nothing yet". */
  readonly filtered: boolean;
  readonly hasMore: boolean;
  readonly isLoadingMore: boolean;
  readonly loadMore: () => void;
}

/** The request params: a value that is empty is not sent. The server searches, filters, counts and sorts. */
function listParams(input: WorkshopListInput, search: WorkshopSearch): ListApplicationsParams {
  const candidates: Record<string, string | undefined> = {
    query: input.query.trim(),
    tags: input.tags.join(','),
    status: (search.statuses ?? []).join(','),
    origin: search.origin,
    folder_id: input.folderId === PINNED_GROUP_ID ? undefined : input.folderId,
    sort_by: search.sort_by === 'name' ? 'name' : 'created_at',
    sort_order: search.sort_order === 'asc' ? 'asc' : 'desc',
  };
  const params: ListApplicationsParams = { agents_type: input.agentsType, limit: PAGE_SIZE };
  for (const [key, value] of Object.entries(candidates)) {
    if (value !== undefined && value !== '') Object.assign(params, { [key]: value });
  }
  // The personal Pinned group rides the folder slot; the server has a flag for it.
  if (input.folderId === PINNED_GROUP_ID) params.pinned = true;
  return params;
}

/**
 * The Workshop's agents or pipelines list (ADR-0051 M1), over one endpoint:
 * the project's rows narrowed, counted and sorted by the server (search, tags,
 * open folder, status and origin facets), fetched a server page at a time, so
 * every row the facet counts claim is reachable with "load more". The first
 * page's `facets` carry the counts the sidebar offers.
 */
export function useWorkshopListData(input: WorkshopListInput): WorkshopListData {
  const search = useSearch({ strict: false }) as WorkshopSearch;
  const params = listParams(input, search);
  const projectId = input.projectId ?? '';

  const list = useInfiniteQuery({
    queryKey: [...getListApplicationsQueryKey(projectId, params), 'workshop'],
    initialPageParam: 0,
    queryFn: async ({ pageParam, signal }) => {
      const envelope = await listApplications(projectId, { ...params, offset: pageParam }, { signal });
      // `.data`'s declared type includes the error-envelope variant, never reachable: `eliteaFetch` throws instead of resolving with it.
      return envelope.data as ApplicationList;
    },
    getNextPageParam: (last, all) => {
      const loaded = all.reduce((count, page) => count + page.rows.length, 0);
      return last.rows.length > 0 && loaded < last.total ? loaded : undefined;
    },
    enabled: input.projectId !== undefined && input.enabled,
  });

  const pages = list.data?.pages;
  const rows = useMemo(() => (pages ?? []).flatMap((page) => page.rows), [pages]);

  return {
    rows,
    wire: pages?.[0],
    isLoading: list.isPending && list.isFetching,
    isError: list.isError,
    filtered: input.query.trim() !== '' || (search.statuses ?? []).length > 0 || (search.origin ?? '') !== '',
    hasMore: list.hasNextPage,
    isLoadingMore: list.isFetchingNextPage,
    loadMore: () => void list.fetchNextPage(),
  };
}
