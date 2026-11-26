import type { ReactNode } from 'react';

/** The direction of a sorted column. */
export type SortDirection = 'asc' | 'desc';

/** The sorted column and its direction (`useTableSort`). */
export interface GridTableSort {
  readonly field: string;
  readonly direction: SortDirection;
}

/**
 * One column of a `GridTable`. The legacy `GridTable` had three cell kinds
 * (name, data, actions) that differed only in their default content; here a
 * column renders its own content with `render`, and without it the cell shows
 * `row[field]` as one line of text with a tooltip when it is cut.
 */
export interface GridTableColumn<Row> {
  readonly field: string;
  readonly label: string;
  /** The column's grid track: `1fr` (the default), `1.5fr`, `9.375rem`. */
  readonly width?: string;
  readonly sortable?: boolean;
  /** `right` puts the header and the cell content at the end (actions, numbers). */
  readonly align?: 'left' | 'right';
  /** The column is hidden when the container is narrower than this, in px (`useResponsiveColumns`). */
  readonly hideBelow?: number;
  readonly render?: (row: Row) => ReactNode;
}

/** The header's select-all checkbox (`useRowSelection`). */
export interface GridTableSelectAll {
  readonly checked: boolean;
  readonly indeterminate: boolean;
  readonly onChange: () => void;
  readonly label: string;
}

/** A row's checkbox (`useRowSelection`). */
export interface GridTableRowSelect {
  readonly checked: boolean;
  readonly onChange: () => void;
  readonly label: string;
}
