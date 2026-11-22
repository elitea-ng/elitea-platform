import { useCallback, useMemo } from 'react';

import type { ListToolkitInstancesParams } from '@/shared/api/generated/model';
import { useListToolkitInstances } from '@/shared/api/generated/toolkits/toolkits';
import { unwrapListPage } from '@/shared/api/unwrap';

import { PICKER_PAGE_SIZE } from './toolPicker.constants';
import type { PickerListState, ToolkitListRow } from './toolPicker.types';
import { usePagedPickerAccumulator, usePagedPickerPagination } from './usePagedPickerList';

export interface UseProjectToolkitsListParams {
  readonly mcp?: boolean | undefined;
  readonly query?: string | undefined;
  readonly skip?: boolean | undefined;
  readonly projectId?: string | undefined;
}

/**
 * Project toolkits (or MCPs) for a picker: server-side search and infinite scroll.
 */
export function useProjectToolkitsList({
  mcp = false,
  query = '',
  skip = false,
  projectId: projectIdOverride,
}: UseProjectToolkitsListParams): PickerListState<ToolkitListRow> {
  const { projectId, page, offset, limit, enabled, setPage } = usePagedPickerPagination({
    pageSize: PICKER_PAGE_SIZE,
    query,
    skip,
    projectIdOverride,
  });

  const params: ListToolkitInstancesParams = {
    limit,
    offset,
    mcp,
    ...(query ? { query } : {}),
  };

  const result = useListToolkitInstances(projectId, params, {
    query: { enabled },
  });

  const unwrapped = useMemo(
    () => unwrapListPage<ToolkitListRow>(result.data, 'toolPicker.listToolkitInstances'),
    [result.data],
  );

  const refetch = useCallback(() => {
    void result.refetch();
  }, [result]);

  return usePagedPickerAccumulator<ToolkitListRow>({
    page,
    pageSize: PICKER_PAGE_SIZE,
    queryKey: `${projectId}:${String(mcp)}:${query}`,
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
