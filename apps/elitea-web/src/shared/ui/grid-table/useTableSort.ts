import { useCallback, useState } from 'react';

import type { GridTableSort, SortDirection } from './model';

/** Compares two cell values; `a` and `b` are the rows, for a comparator that needs another field. */
export type SortComparator<Row> = (aValue: unknown, bValue: unknown, a: Row, b: Row) => number;

/**
 * Comparators for values the default comparison sorts wrong. Ported from
 * `apps/elitea-ui/src/[fsd]/entities/grid-table/lib/utils/sortComparators.js`
 * (`date` and `number`; `fileSize` and `boolean` come with their first
 * screen).
 */
export const SortComparators = {
  /** An ISO date; an empty or unreadable date sorts first. */
  date: (a: unknown, b: unknown): number => toTime(a) - toTime(b),
  number: (a: unknown, b: unknown): number => toNumber(a) - toNumber(b),
};

function toTime(value: unknown): number {
  if (typeof value !== 'string' || value === '') return 0;
  const time = new Date(value).getTime();
  return Number.isNaN(time) ? 0 : time;
}

function toNumber(value: unknown): number {
  if (typeof value === 'number') return value;
  const parsed = typeof value === 'string' ? Number.parseFloat(value) : Number.NaN;
  return Number.isNaN(parsed) ? 0 : parsed;
}

function compareDefault(aValue: unknown, bValue: unknown, direction: SortDirection): number {
  // An empty value goes last when ascending and first when descending, as in the legacy hook.
  if (aValue == null && bValue == null) return 0;
  if (aValue == null) return direction === 'asc' ? 1 : -1;
  if (bValue == null) return direction === 'asc' ? -1 : 1;
  const a = typeof aValue === 'string' ? aValue.toLowerCase() : aValue;
  const b = typeof bValue === 'string' ? bValue.toLowerCase() : bValue;
  const result = typeof a === 'string' && typeof b === 'string' ? a.localeCompare(b) : Number(a) - Number(b);
  return direction === 'asc' ? result : -result;
}

export interface UseTableSortOptions<Row> {
  readonly defaultField?: string;
  readonly defaultDirection?: SortDirection;
  /** A comparator per field, for values that are not plain text or numbers. */
  readonly comparators?: Readonly<Record<string, SortComparator<Row>>>;
}

/**
 * The sorted column and a sort of the rows by it. Ported from
 * `apps/elitea-ui/src/[fsd]/entities/grid-table/lib/hooks/useTableSort.hooks.js`:
 * a click on the sorted column flips the direction, a click on another column
 * sorts it ascending.
 */
export function useTableSort<Row extends object>({
  defaultField = 'name',
  defaultDirection = 'asc',
  comparators = {},
}: UseTableSortOptions<Row> = {}) {
  const [sort, setSort] = useState<GridTableSort>({ field: defaultField, direction: defaultDirection });

  const handleSort = useCallback((field: string) => {
    setSort((previous) => ({
      field,
      direction: previous.field === field && previous.direction === 'asc' ? 'desc' : 'asc',
    }));
  }, []);

  const sortRows = useCallback(
    (rows: readonly Row[]): Row[] => {
      const { field, direction } = sort;
      const comparator = comparators[field];
      return [...rows].sort((a, b) => {
        const aValue = (a as Record<string, unknown>)[field];
        const bValue = (b as Record<string, unknown>)[field];
        if (comparator === undefined) return compareDefault(aValue, bValue, direction);
        const result = comparator(aValue, bValue, a, b);
        return direction === 'asc' ? result : -result;
      });
    },
    [sort, comparators],
  );

  return { sort, handleSort, sortRows };
}
