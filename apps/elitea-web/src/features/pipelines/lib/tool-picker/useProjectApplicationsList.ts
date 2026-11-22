import { useCallback, useMemo } from 'react';

import { useListApplications } from '@/shared/api/generated/applications/applications';
import type { ListApplicationsParams } from '@/shared/api/generated/model';
import { unwrapListPage } from '@/shared/api/unwrap';

import { PICKER_PAGE_SIZE } from './toolPicker.constants';
import type { ApplicationListRow, PickerListState } from './toolPicker.types';
import { usePagedPickerAccumulator, usePagedPickerPagination } from './usePagedPickerList';

export interface UseProjectApplicationsListParams {
  readonly agentsType: 'classic' | 'pipeline';
  readonly query?: string | undefined;
  readonly skip?: boolean | undefined;
  readonly projectId?: string | undefined;
}

/**
 * Project agents (`agentsType: 'classic'`) or pipelines (`'pipeline'`) for a picker:
 * server-side search and infinite scroll.
 */
export function useProjectApplicationsList({
  agentsType,
  query = '',
  skip = false,
  projectId: projectIdOverride,
}: UseProjectApplicationsListParams): PickerListState<ApplicationListRow> {
  const { projectId, page, offset, limit, enabled, setPage } = usePagedPickerPagination({
    pageSize: PICKER_PAGE_SIZE,
    query,
    skip,
    projectIdOverride,
  });

  const params: ListApplicationsParams = {
    limit,
    offset,
    agents_type: agentsType,
    sort_by: 'created_at',
    sort_order: 'desc',
    ...(query ? { query } : {}),
  };

  const result = useListApplications(projectId, params, {
    query: { enabled },
  });

  const unwrapped = useMemo(
    () => unwrapListPage<ApplicationListRow>(result.data, 'toolPicker.listApplications'),
    [result.data],
  );

  const refetch = useCallback(() => {
    void result.refetch();
  }, [result]);

  return usePagedPickerAccumulator<ApplicationListRow>({
    page,
    pageSize: PICKER_PAGE_SIZE,
    queryKey: `${projectId}:${agentsType}:${query}`,
    enabled,
    pageRows: unwrapped.rows,
    total: unwrapped.total,
    isLoading: result.isLoading,
    isFetching: result.isFetching,
    isError: result.isError,
    refetch,
    setPage,
  });
}
