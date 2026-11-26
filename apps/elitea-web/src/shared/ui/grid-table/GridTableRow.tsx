import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { BaseCheckbox } from '../BaseCheckbox';
import { TypographyWithConditionalTooltip } from '../TypographyWithConditionalTooltip';
import type { GridTableColumn, GridTableRowSelect } from './model';

export interface GridTableRowProps<Row> {
  readonly row: Row;
  readonly columns: readonly GridTableColumn<Row>[];
  /** The grid template the header uses too (`useResponsiveColumns`). */
  readonly gridTemplateColumns: string;
  /** The row's checkbox; absent means the table has no checkbox column. */
  readonly select?: GridTableRowSelect | undefined;
  /** Makes the whole row clickable (open the item). */
  readonly onClick?: (() => void) | undefined;
  readonly 'data-testid'?: string | undefined;
  /** `<prefix>-column-value-<field>` on each cell. */
  readonly cellTestIdPrefix?: string | undefined;
}

/**
 * One row of a `GridTable`. Ported from
 * `apps/elitea-ui/src/[fsd]/entities/grid-table/ui/GridTableRow.jsx` and its
 * cells: a CSS grid row on the header's template, at least `2.5rem` high, a
 * `role.border` rule under it and `role.hover` under the pointer or when
 * selected. A cell is padded 8 / 16 px (scale steps 1 and 2) and holds its
 * column's `render`, or `row[field]` as one line of `bodyMedium` text with a
 * tooltip when it is cut.
 *
 * Deviations from the baseline: the hover is CSS, not a hovered-row state
 * kept by the page; the row's props are the row, its columns and its
 * checkbox — a cell's look comes from its column, not from per-row `sx`.
 */
export function GridTableRow<Row>({
  row,
  columns,
  gridTemplateColumns,
  select,
  onClick,
  'data-testid': dataTestId,
  cellTestIdPrefix,
}: GridTableRowProps<Row>): ReactNode {
  return (
    <Box
      component="tr"
      data-testid={dataTestId}
      data-selected={select?.checked === true ? 'true' : undefined}
      sx={rowSx(gridTemplateColumns, onClick !== undefined)}
      onClick={onClick}
    >
      {select !== undefined && (
        <Box
          component="td"
          sx={checkboxCellSx}
          onClick={(event) => {
            event.stopPropagation();
          }}
        >
          <BaseCheckbox
            size="small"
            checked={select.checked}
            aria-label={select.label}
            onChange={select.onChange}
          />
        </Box>
      )}
      {columns.map((column) => (
        <Box
          key={column.field}
          component="td"
          data-testid={cellTestIdPrefix === undefined ? undefined : `${cellTestIdPrefix}-column-value-${column.field}`}
          sx={cellSx(column.align === 'right')}
        >
          <CellContent
            column={column}
            row={row}
          />
        </Box>
      ))}
    </Box>
  );
}

function CellContent<Row>({ column, row }: { readonly column: GridTableColumn<Row>; readonly row: Row }): ReactNode {
  const content = column.render === undefined ? textOf((row as Record<string, unknown>)[column.field]) : column.render(row);
  if (typeof content !== 'string') return content;
  return (
    <TypographyWithConditionalTooltip
      title={content}
      placement="top"
      variant="bodyMedium"
      sx={cellTextSx}
    >
      {content}
    </TypographyWithConditionalTooltip>
  );
}

/** The legacy `value ?? '-'`, for a value that is text or a number. */
function textOf(value: unknown): string {
  if (typeof value === 'string' && value !== '') return value;
  if (typeof value === 'number') return String(value);
  return '-';
}

function rowSx(gridTemplateColumns: string, clickable: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'grid',
    gridTemplateColumns,
    alignItems: 'center',
    width: '100%',
    minHeight: '2.5rem',
    flexShrink: 0,
    cursor: clickable ? 'pointer' : 'default',
    borderBottom: `0.0625rem solid ${theme.vars.palette.role.border}`,
    transition: 'background-color 0.2s ease',
    '&:hover, &[data-selected="true"]': { backgroundColor: theme.vars.palette.role.hover },
    '&:first-of-type': { borderTopLeftRadius: theme.vars.shape.radiusMd, borderTopRightRadius: theme.vars.shape.radiusMd },
    '&:last-of-type': {
      borderBottom: 'none',
      borderBottomLeftRadius: theme.vars.shape.radiusMd,
      borderBottomRightRadius: theme.vars.shape.radiusMd,
    },
  });
}

const checkboxCellSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'center', minWidth: 0 };

function cellSx(alignEnd: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    justifyContent: alignEnd ? 'flex-end' : 'flex-start',
    gap: 1,
    paddingY: 1,
    paddingX: 2,
    minWidth: 0,
    overflow: 'hidden',
    color: theme.vars.palette.role.text,
  });
}

const cellTextSx: SxProps<Theme> = { flex: 1, minWidth: 0 };
