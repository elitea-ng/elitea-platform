import type { ReactNode } from 'react';

import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import UnfoldMoreIcon from '@mui/icons-material/UnfoldMore';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { BaseCheckbox } from '../BaseCheckbox';
import type { GridTableColumn, GridTableSelectAll, GridTableSort, SortDirection } from './model';

export interface GridTableHeaderProps<Row> {
  readonly columns: readonly GridTableColumn<Row>[];
  /** The grid template the rows use too (`useResponsiveColumns`). */
  readonly gridTemplateColumns: string;
  readonly sort?: GridTableSort | undefined;
  /** Called with the field of a sortable column the user clicks. */
  readonly onSort?: ((field: string) => void) | undefined;
  /** The select-all checkbox; absent means the table has no checkbox column. */
  readonly selectAll?: GridTableSelectAll | undefined;
  /** `<prefix>-column-header-<field>` on each header cell. */
  readonly testIdPrefix?: string | undefined;
}

/**
 * The header bar of a `GridTable`. Ported from
 * `apps/elitea-ui/src/[fsd]/entities/grid-table/ui/GridTableHeader.jsx`:
 * a `2.25rem` bar on `role.hover` with a `role.border` outline, `labelMedium`
 * labels and a short divider between the cells.
 *
 * Deviations from the baseline:
 *  - The sort control is a real button and the cell carries `aria-sort`, so
 *    the keyboard sorts and a screen reader hears the order.
 *  - The sort icon says the direction (issue 2050, as `EntityListHeaderCell`
 *    already does): the legacy double arrow looks the same both ways.
 *  - A label reads `role.textMuted` and the sorted one `role.text`, instead
 *    of the legacy `opacity: 0.7`, which under 4.5:1 in the light scheme.
 *  - No `density="compact"` prop: compact density comes from the theme.
 */
export function GridTableHeader<Row>({
  columns,
  gridTemplateColumns,
  sort,
  onSort,
  selectAll,
  testIdPrefix,
}: GridTableHeaderProps<Row>): ReactNode {
  return (
    <Box component="thead">
      <Box
        component="tr"
        sx={headerSx(gridTemplateColumns)}
      >
        {selectAll !== undefined && (
          <Box
            component="th"
            scope="col"
            sx={checkboxCellSx}
          >
            <BaseCheckbox
              size="small"
              checked={selectAll.checked}
              indeterminate={selectAll.indeterminate}
              aria-label={selectAll.label}
              onChange={selectAll.onChange}
            />
          </Box>
        )}
        {columns.map((column, index) => {
          const active = sort?.field === column.field;
          const sortable = column.sortable === true && onSort !== undefined;
          const ariaSort = ariaSortOf(sortable && active ? sort?.direction : undefined);
          const label = (
            <Typography
              variant="labelMedium"
              sx={labelSx(active)}
            >
              {column.label}
            </Typography>
          );
          return (
            <Box
              key={column.field}
              component="th"
              scope="col"
              aria-sort={ariaSort}
              data-testid={testIdPrefix === undefined ? undefined : `${testIdPrefix}-column-header-${column.field}`}
              sx={headerCellSx(column.align === 'right', index < columns.length - 1)}
            >
              {sortable ? (
                <Box
                  component="button"
                  type="button"
                  sx={sortButtonSx}
                  onClick={() => {
                    onSort(column.field);
                  }}
                >
                  {label}
                  <SortIcon
                    active={active}
                    descending={sort?.direction === 'desc'}
                  />
                </Box>
              ) : (
                label
              )}
            </Box>
          );
        })}
      </Box>
    </Box>
  );
}

/** `aria-sort` of a header cell: the direction of the sorted column, `none` for any other. */
function ariaSortOf(direction: SortDirection | undefined): 'ascending' | 'descending' | 'none' {
  if (direction === undefined) return 'none';
  return direction === 'desc' ? 'descending' : 'ascending';
}

function SortIcon({ active, descending }: { readonly active: boolean; readonly descending: boolean }): ReactNode {
  const Icon = !active ? UnfoldMoreIcon : descending ? ArrowDownwardIcon : ArrowUpwardIcon;
  return (
    <Icon
      data-testid="grid-table-sort-icon"
      sx={{ width: '1rem', height: '1rem', flexShrink: 0, opacity: active ? 1 : 0.5 }}
    />
  );
}

function headerSx(gridTemplateColumns: string): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'grid',
    gridTemplateColumns,
    alignItems: 'stretch',
    width: '100%',
    height: '2.25rem',
    backgroundColor: theme.vars.palette.role.hover,
    border: `0.0625rem solid ${theme.vars.palette.role.border}`,
    borderRadius: theme.vars.shape.radiusMd,
    overflow: 'hidden',
  });
}

const checkboxCellSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 0.5 };

function headerCellSx(alignEnd: boolean, divided: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    position: 'relative',
    display: 'flex',
    alignItems: 'center',
    justifyContent: alignEnd ? 'flex-end' : 'flex-start',
    paddingY: 0.5,
    paddingX: 1.5,
    minWidth: 0,
    overflow: 'hidden',
    ...(divided && {
      '&::after': {
        content: '""',
        position: 'absolute',
        right: 0,
        top: '50%',
        transform: 'translateY(-50%)',
        width: '0.0625rem',
        height: '1.25rem',
        backgroundColor: theme.vars.palette.role.border,
      },
    }),
  });
}

const sortButtonSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: 0.5,
  minWidth: 0,
  padding: 0,
  margin: 0,
  border: 'none',
  background: 'none',
  color: 'inherit',
  font: 'inherit',
  cursor: 'pointer',
  '&:focus-visible': { outline: `0.125rem solid ${theme.vars.palette.role.signal}`, outlineOffset: '0.125rem' },
});

function labelSx(active: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    color: active ? theme.vars.palette.role.text : theme.vars.palette.role.textMuted,
    transition: 'color 0.2s ease',
    'th:hover &': { color: theme.vars.palette.role.text },
    whiteSpace: 'nowrap',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
  });
}
