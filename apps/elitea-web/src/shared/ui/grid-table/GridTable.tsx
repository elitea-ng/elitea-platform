import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

export interface GridTableProps {
  /** `GridTableHeader`, then `GridTableBody`. */
  readonly children: ReactNode;
  /** Shows `loadingMessage` in place of the table. */
  readonly isLoading?: boolean;
  /** Shows `emptyMessage` in place of the table. */
  readonly isEmpty?: boolean;
  readonly emptyMessage?: ReactNode;
  readonly loadingMessage?: ReactNode;
  readonly 'data-testid'?: string | undefined;
}

/**
 * The shared table of ELITEA NG (ADR-0062 decision 4, the table family; D35:
 * the legacy `GridTable`, not a new design). Ported from
 * `apps/elitea-ui/src/[fsd]/entities/grid-table/ui/GridTableContainer.jsx`:
 * the loading and the empty message in place of the table, and a `12px` gap
 * between the header bar and the rows (scale step 1.5).
 *
 * The parts are composed by the screen, as in the legacy app:
 *
 *   <GridTable isEmpty={rows.length === 0} emptyMessage="No tokens">
 *     <GridTableHeader columns={…} gridTemplateColumns={…} sort={…} onSort={…} />
 *     <GridTableBody>{rows.map((row) => <GridTableRow key={…} row={row} … />)}</GridTableBody>
 *   </GridTable>
 *
 * The elements are `table`, `thead`, `tbody`, `tr`, `th` and `td` drawn as a
 * CSS grid, so a screen reader reads a table (the legacy one was `div`s).
 * Pagination is `EntityListPagination`, the port of `GridTablePagination`.
 *
 * Deviations from the baseline: no `toolbar` slot and no outer padding — the
 * page frame owns both; the messages are `bodyMedium` in `role.textMuted`.
 */
export function GridTable({
  children,
  isLoading = false,
  isEmpty = false,
  emptyMessage = 'No data',
  loadingMessage = 'Loading...',
  'data-testid': dataTestId,
}: GridTableProps): ReactNode {
  if (isLoading || isEmpty) {
    return (
      <Box
        data-testid={dataTestId}
        sx={stateSx}
      >
        <Typography
          variant="bodyMedium"
          color="role.textMuted"
        >
          {isLoading ? loadingMessage : emptyMessage}
        </Typography>
      </Box>
    );
  }
  return (
    <Box
      component="table"
      data-testid={dataTestId}
      sx={tableSx}
    >
      {children}
    </Box>
  );
}

export interface GridTableBodyProps {
  /** `GridTableRow`s. */
  readonly children: ReactNode;
  readonly 'data-testid'?: string | undefined;
}

/**
 * The rows. Ported from `GridTableBody.jsx`: a column that scrolls when the
 * table is given less height than its rows. No `minHeight` default: the
 * legacy `20rem` kept the pagination low on a short page; a page that wants
 * it sets the height of the table's parent.
 */
export function GridTableBody({ children, 'data-testid': dataTestId }: GridTableBodyProps): ReactNode {
  return (
    <Box
      component="tbody"
      data-testid={dataTestId}
      sx={bodySx}
    >
      {children}
    </Box>
  );
}

const tableSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: 1.5,
  width: '100%',
  minHeight: 0,
  flex: 1,
  borderCollapse: 'separate',
};

const bodySx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  flex: 1,
  minHeight: 0,
  overflow: 'auto',
};

const stateSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 6 };
