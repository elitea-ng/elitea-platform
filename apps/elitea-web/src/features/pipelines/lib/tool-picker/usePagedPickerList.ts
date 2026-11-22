import { useCallback, useEffect, useMemo, useState } from 'react';

import { useSelectedProjectId } from '../flow-editor/hooks/useSelectedProjectId';
import type { PickerListState } from './toolPicker.types';

export interface UsePagedPickerPaginationParams {
  readonly pageSize: number;
  readonly query?: string | undefined;
  readonly skip?: boolean | undefined;
  readonly projectIdOverride?: string | undefined;
}

export interface UsePagedPickerPaginationResult {
  readonly projectId: string;
  readonly page: number;
  readonly offset: number;
  readonly limit: number;
  readonly enabled: boolean;
  readonly setPage: React.Dispatch<React.SetStateAction<number>>;
}

/**
 * Manages page offset state for a picker list query, resetting synchronously to page 0
 * whenever `projectId` or `query` changes.
 */
export function usePagedPickerPagination({
  pageSize,
  query = '',
  skip = false,
  projectIdOverride,
}: UsePagedPickerPaginationParams): UsePagedPickerPaginationResult {
  const routeProjectId = useSelectedProjectId();
  const projectId = projectIdOverride ?? routeProjectId ?? '';

  const [page, setPage] = useState(0);
  const pageKey = `${projectId}:${query}`;
  const [prevPageKey, setPrevPageKey] = useState(pageKey);

  if (pageKey !== prevPageKey) {
    setPrevPageKey(pageKey);
    setPage(0);
  }

  return {
    projectId,
    page,
    offset: page * pageSize,
    limit: pageSize,
    enabled: Boolean(projectId) && !skip,
    setPage,
  };
}

export interface UsePagedPickerAccumulatorParams<TRow> {
  readonly page: number;
  readonly pageSize: number;
  readonly queryKey: string;
  readonly enabled: boolean;
  readonly pageRows: readonly TRow[];
  readonly total: number;
  readonly isLoading: boolean;
  readonly isFetching: boolean;
  readonly isError: boolean;
  readonly refetch: () => void;
  readonly setPage: React.Dispatch<React.SetStateAction<number>>;
}

/**
 * Accumulates loaded pages into `rows`, resets synchronously on `queryKey` change,
 * and exposes `hasMore`, `onLoadMore`, and `reload`.
 */
export function usePagedPickerAccumulator<TRow>({
  page,
  pageSize,
  queryKey,
  enabled,
  pageRows,
  total,
  isLoading,
  isFetching,
  isError,
  refetch,
  setPage,
}: UsePagedPickerAccumulatorParams<TRow>): PickerListState<TRow> {
  const [pagesByNumber, setPagesByNumber] = useState<Readonly<Record<number, readonly TRow[]>>>({});
  const [prevKey, setPrevKey] = useState(queryKey);

  if (queryKey !== prevKey) {
    setPrevKey(queryKey);
    setPagesByNumber({});
  }

  useEffect(() => {
    if (!enabled || isFetching || isError) return;
    setPagesByNumber((prev) => ({ ...prev, [page]: pageRows }));
  }, [enabled, isError, isFetching, page, pageRows]);

  const rows = useMemo(() => {
    const keys = Object.keys(pagesByNumber)
      .map(Number)
      .sort((a, b) => a - b);
    if (keys.length === 0) return page === 0 ? pageRows : [];
    return keys.flatMap((pageNum) => pagesByNumber[pageNum] ?? []);
  }, [page, pageRows, pagesByNumber]);

  const numericTotal = Number(total) || 0;
  const hasMore = (page + 1) * pageSize < numericTotal;

  const onLoadMore = useCallback(() => {
    if (!isFetching && hasMore) {
      setPage((prev) => prev + 1);
    }
  }, [hasMore, isFetching, setPage]);

  const reload = useCallback(() => {
    if (page === 0) {
      refetch();
    } else {
      setPage(0);
    }
  }, [page, refetch, setPage]);

  return {
    rows,
    total: numericTotal,
    isLoading,
    isFetching,
    isError,
    hasMore,
    onLoadMore,
    reload,
  };
}
