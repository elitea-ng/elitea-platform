import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { SortArrowsIcon } from '@/shared/ui/icons/sort-arrows-icon';

import type { ArtifactColumn, ArtifactSortField } from '../model/columns';

interface ArtifactGridHeaderProps {
  readonly columns: readonly ArtifactColumn[];
  readonly gridTemplateColumns: string;
  readonly sortField: ArtifactSortField;
  readonly sortDirection: 'asc' | 'desc';
  readonly allSelected: boolean;
  readonly someSelected: boolean;
  readonly onSelectAll: () => void;
  readonly onSort: (field: ArtifactSortField) => void;
}

/**
 * The table's header strip: a rounded, bordered 2.25rem bar laid out on the
 * same CSS grid as the rows, with the sort glyph BEFORE each label and a
 * hairline divider on every column boundary.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/entities/grid-table/ui/
 * GridTableHeader.jsx`. It is a grid, not a `<thead>`: the row and the header
 * share one `gridTemplateColumns` string, which is what keeps the two aligned
 * when a column is dropped at a narrow width.
 */
export function ArtifactGridHeader(props: ArtifactGridHeaderProps): ReactNode {
  return (
    <Box
      component="tr"
      sx={headerSx(props.gridTemplateColumns)}
    >
      <Box
        component="th"
        sx={checkboxCellSx}
      >
        <BaseCheckbox
          checked={props.allSelected}
          indeterminate={!props.allSelected && props.someSelected}
          aria-label={t('artifacts.table.selectAll', 'Select all artifacts')}
          onChange={props.onSelectAll}
        />
        <Box sx={dividerSx} />
      </Box>
      {props.columns.map((column, index) => {
        const isLast = index === props.columns.length - 1;
        const isActive = props.sortField === column.field;
        const sortField = column.field === 'actions' ? undefined : column.field;
        return (
          <Box
            key={column.field}
            component="th"
            sx={cellSx(isActive, column.sortable)}
          >
            {sortField !== undefined && column.sortable && (
              <Box
                component="button"
                type="button"
                aria-label={column.label}
                sx={sortButtonSx}
                onClick={() => props.onSort(sortField)}
              >
                <SortArrowsIcon
                  style={{
                    ...sortIconStyle,
                    transform: isActive && props.sortDirection === 'desc' ? 'rotate(180deg)' : 'rotate(0deg)',
                  }}
                />
              </Box>
            )}
            <Typography
              variant="labelMedium"
              sx={labelSx}
            >
              {column.label}
            </Typography>
            {!isLast && <Box sx={dividerSx} />}
          </Box>
        );
      })}
    </Box>
  );
}

const sortIconStyle = { width: '1rem', height: '1rem', transition: 'transform 0.2s ease' };
const headerSx = (gridTemplateColumns: string): SxProps<Theme> => (theme) => ({
  display: 'grid',
  gridTemplateColumns,
  alignItems: 'stretch',
  width: '100%',
  height: '2.25rem',
  flexShrink: 0,
  backgroundColor: theme.vars.palette.background.userInputBackground,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  borderRadius: theme.vars.shape.radiusMd,
  overflow: 'hidden',
});
const checkboxCellSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  padding: theme.spacing(0.5),
  position: 'relative',
});
const cellSx = (isActive: boolean, sortable: boolean): SxProps<Theme> => (theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.5),
  padding: theme.spacing(0.5, 1.5),
  position: 'relative',
  minWidth: 0,
  // `<th>` centres its text by UA default; every baseline header cell is
  // left-aligned next to its sort glyph.
  textAlign: 'start',
  fontWeight: 'inherit',
  overflow: 'hidden',
  cursor: sortable ? 'pointer' : 'default',
  opacity: isActive ? 1 : 0.7,
  transition: 'opacity 0.2s ease',
  '&:hover': { opacity: 1 },
});
const sortButtonSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  padding: theme.spacing(0.75),
  border: 0,
  background: 'transparent',
  borderRadius: theme.vars.shape.radiusLg,
  cursor: 'pointer',
});
// See `ArtifactGridRow.tsx`'s `cellTextSx` for why the colour is in `sx`.
const labelSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  flex: 1,
  minWidth: 0,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
});
const dividerSx: SxProps<Theme> = (theme) => ({
  position: 'absolute',
  right: 0,
  top: '50%',
  transform: 'translateY(-50%)',
  width: '0.0625rem',
  height: '1.25rem',
  backgroundColor: theme.vars.palette.divider,
});
