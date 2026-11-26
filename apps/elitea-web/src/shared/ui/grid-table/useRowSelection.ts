import { useCallback, useMemo, useState } from 'react';

import type { GridTableSelectAll } from './model';

export interface UseRowSelectionOptions<Row> {
  /** The rows on the current page. */
  readonly rows: readonly Row[];
  readonly getRowId: (row: Row) => string;
  readonly initialSelected?: readonly string[];
}

/**
 * The selected rows and the header's select-all state. Ported from
 * `apps/elitea-ui/src/[fsd]/entities/grid-table/lib/hooks/useRowSelection.hooks.js`:
 * the selection is kept across pages, and select-all adds or removes only the
 * rows of the current page.
 */
export function useRowSelection<Row>({ rows, getRowId, initialSelected = [] }: UseRowSelectionOptions<Row>) {
  const [selectedIds, setSelectedIds] = useState<readonly string[]>(initialSelected);

  const pageIds = useMemo(() => rows.map(getRowId), [rows, getRowId]);
  const selected = useMemo(() => new Set(selectedIds), [selectedIds]);
  const pageSelected = pageIds.filter((id) => selected.has(id)).length;
  const isAllSelected = pageIds.length > 0 && pageSelected === pageIds.length;

  const toggleAll = useCallback(() => {
    setSelectedIds((previous) => {
      const page = new Set(pageIds);
      if (isAllSelected) return previous.filter((id) => !page.has(id));
      const kept = new Set(previous);
      return [...previous, ...pageIds.filter((id) => !kept.has(id))];
    });
  }, [isAllSelected, pageIds]);

  const toggleRow = useCallback((id: string) => {
    setSelectedIds((previous) => (previous.includes(id) ? previous.filter((other) => other !== id) : [...previous, id]));
  }, []);

  const clearSelection = useCallback(() => {
    setSelectedIds([]);
  }, []);

  const selectAll = (label: string): GridTableSelectAll => ({
    checked: isAllSelected,
    indeterminate: pageSelected > 0 && !isAllSelected,
    onChange: toggleAll,
    label,
  });

  return { selectedIds, isSelected: (id: string) => selected.has(id), toggleRow, toggleAll, clearSelection, selectAll };
}
