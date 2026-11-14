import { useEffect, useMemo } from 'react';

import type { ToolkitListItem } from '@/features/toolkits';
import type { FolderFilter } from '@/widgets/entity-folders';

interface PagedList {
  readonly hasMore: boolean;
  readonly onLoadMore: () => void;
}

/**
 * Narrows the loaded rows to the open project folder (issue 2048). The
 * toolkits endpoint has no folder parameter, so while a folder is open the
 * remaining pages are loaded as well, and the folder is never short of the
 * entities that sit on a page the person has not scrolled to.
 */
export function useFolderRows(rows: readonly ToolkitListItem[], folderFilter: FolderFilter, list: PagedList): readonly ToolkitListItem[] {
  const { active } = folderFilter;
  const { hasMore, onLoadMore } = list;
  useEffect(() => {
    if (active && hasMore) onLoadMore();
  }, [active, hasMore, onLoadMore]);
  return useMemo(() => rows.filter((item) => folderFilter.includes(item.id)), [rows, folderFilter]);
}
