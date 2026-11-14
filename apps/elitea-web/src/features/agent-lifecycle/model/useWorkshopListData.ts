import { useEffect, useMemo, useState } from 'react';

import { useSearch } from '@tanstack/react-router';

import { useListApplications } from '@/shared/api/generated/applications/applications';
import type { Application, ApplicationList, ListApplicationsParams } from '@/shared/api/generated/model';

const PAGE_SIZE = 20;
/** The server's page cap: the list asks for as much of the project as one response may hold. */
const FETCH_LIMIT = 100;

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
  /** The page of rows on screen: matching the search, sorted, and cut to the "load more" window. */
  readonly rows: readonly Application[];
  readonly wire: ApplicationList | undefined;
  readonly isLoading: boolean;
  readonly isError: boolean;
  /** A search or a filter is on, so an empty list means "nothing found" and not "nothing yet". */
  readonly filtered: boolean;
  readonly hasMore: boolean;
  readonly loadMore: () => void;
}

/** The request params: a value that is empty is not sent. */
function listParams(input: WorkshopListInput, search: WorkshopSearch): ListApplicationsParams {
  const status = search.statuses?.[0];
  const origin = search.origin;
  const candidates: Record<string, string | undefined> = {
    query: input.query.trim(),
    tags: input.tags.join(','),
    status,
    origin,
    folder_id: input.folderId,
  };
  const params: ListApplicationsParams = { agents_type: input.agentsType, limit: FETCH_LIMIT };
  for (const [key, value] of Object.entries(candidates)) {
    if (value !== undefined && value !== '') Object.assign(params, { [key]: value });
  }
  return params;
}

function sortRows(rows: readonly Application[], search: WorkshopSearch): Application[] {
  const byName = search.sort_by === 'name';
  const key = (row: Application): string => (byName ? row.name.trim() || 'Untitled' : row.created_at);
  const sorted = [...rows].sort((a, b) => key(a).localeCompare(key(b)));
  return search.sort_order === 'asc' ? sorted : sorted.reverse();
}

/**
 * The Workshop's agents or pipelines list (ADR-0051 M1), over one endpoint:
 * the project's rows narrowed by the search, the tags, the open folder and the
 * status and origin facets (all of them by the server), then sorted and cut to
 * the "load more" window. The response's `facets` carry the counts the sidebar
 * offers.
 */
export function useWorkshopListData(input: WorkshopListInput): WorkshopListData {
  const search = useSearch({ strict: false }) as WorkshopSearch;
  const [visibleCount, setVisibleCount] = useState(PAGE_SIZE);
  const params = listParams(input, search);
  const paramsKey = JSON.stringify(params);

  // A new search, filter or folder restarts the "load more" window.
  useEffect(() => {
    setVisibleCount(PAGE_SIZE);
  }, [paramsKey]);

  const listQuery = useListApplications(input.projectId ?? '', params, { query: { enabled: input.projectId !== undefined && input.enabled } });
  // `.data.data`'s declared type includes the error-envelope variant, never reachable: `eliteaFetch` throws instead of resolving with it.
  const wire = listQuery.data?.data as ApplicationList | undefined;

  const needle = input.query.trim().toLowerCase();
  const sorted = useMemo(
    () => sortRows((wire?.rows ?? []).filter((row) => row.name.toLowerCase().includes(needle)), search),
    [wire, needle, search],
  );
  const rows = useMemo(() => sorted.slice(0, visibleCount), [sorted, visibleCount]);

  return {
    rows,
    wire,
    isLoading: listQuery.isFetching && wire === undefined,
    isError: listQuery.isError,
    filtered: needle !== '' || search.statuses?.[0] !== undefined || (search.origin ?? '') !== '',
    hasMore: visibleCount < sorted.length,
    loadMore: () => setVisibleCount((count) => count + PAGE_SIZE),
  };
}
