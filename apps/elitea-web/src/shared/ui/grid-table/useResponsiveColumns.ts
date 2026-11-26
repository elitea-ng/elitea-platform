import { useMemo } from 'react';

import type { GridTableColumn } from './model';

/** The checkbox column's track, as in the legacy `GridTable`. */
const CHECKBOX_TRACK = '3rem';

/**
 * The columns that fit the container and the grid template the header and the
 * rows share. Pure, for a caller outside React and for the test.
 */
export function responsiveColumns<Row>(
  columns: readonly GridTableColumn<Row>[],
  containerWidth: number,
  showCheckbox: boolean,
): { visibleColumns: GridTableColumn<Row>[]; gridTemplateColumns: string } {
  const visibleColumns = columns.filter((column) => column.hideBelow === undefined || containerWidth >= column.hideBelow);
  const tracks = visibleColumns.map((column) => column.width ?? '1fr');
  return {
    visibleColumns,
    gridTemplateColumns: (showCheckbox ? [CHECKBOX_TRACK, ...tracks] : tracks).join(' '),
  };
}

export interface UseResponsiveColumnsOptions<Row> {
  readonly columns: readonly GridTableColumn<Row>[];
  /** The width the table has, in px; without it every column shows. */
  readonly containerWidth?: number;
  readonly showCheckbox?: boolean;
}

/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/entities/grid-table/lib/hooks/useResponsiveColumns.hooks.js`.
 * The legacy hook gave the `actions` field its own width option; here every
 * column, actions included, states its `width`.
 */
export function useResponsiveColumns<Row>({
  columns,
  containerWidth = Number.POSITIVE_INFINITY,
  showCheckbox = false,
}: UseResponsiveColumnsOptions<Row>) {
  return useMemo(() => responsiveColumns(columns, containerWidth, showCheckbox), [columns, containerWidth, showCheckbox]);
}
