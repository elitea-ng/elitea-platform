import type { ReactNode } from 'react';

import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import UnfoldMoreIcon from '@mui/icons-material/UnfoldMore';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

interface HeaderCellProps {
  readonly label: string;
  readonly active?: boolean;
  readonly descending?: boolean;
  readonly onClick?: () => void;
}

/** Issue 2050: a sortable column says so, and which way it is sorted. */
function SortIcon({ active, descending }: { readonly active: boolean; readonly descending: boolean }): ReactNode {
  const Icon = !active ? UnfoldMoreIcon : descending ? ArrowDownwardIcon : ArrowUpwardIcon;
  return (
    <Icon
      data-testid="entity-list-sort-icon"
      sx={{ width: '1rem', height: '1rem', flexShrink: 0, opacity: active ? 1 : 0.5 }}
    />
  );
}

export function HeaderCell({ label, active = false, descending = true, onClick }: HeaderCellProps): ReactNode {
  const sortable = onClick !== undefined;
  const ariaSort = !sortable || !active ? 'none' : descending ? 'descending' : 'ascending';
  const text = (
    <Typography
      variant="labelMedium"
      sx={headerTextSx(active)}
    >
      {label}
    </Typography>
  );
  return (
    <Box
      component="th"
      scope="col"
      aria-sort={ariaSort}
      sx={headerCellSx(sortable)}
    >
      {sortable ? (
        // A real button, so the header is focusable and Enter/Space sort.
        <Box
          component="button"
          type="button"
          data-testid="entity-list-sort-button"
          sx={sortButtonSx}
          onClick={onClick}
        >
          {text}
          <SortIcon
            active={active}
            descending={descending}
          />
        </Box>
      ) : (
        text
      )}
    </Box>
  );
}

const sortButtonSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.5),
  minWidth: 0,
  padding: 0,
  margin: 0,
  border: 'none',
  background: 'none',
  color: 'inherit',
  font: 'inherit',
  cursor: 'pointer',
  '&:focus-visible': { outline: `0.125rem solid ${theme.vars.palette.border.lines}`, outlineOffset: '0.125rem' },
});


function headerCellSx(sortable: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    gap: theme.spacing(0.5),
    padding: `${theme.spacing(0.5)} ${theme.spacing(1.5)}`,
    minWidth: 0,
    overflow: 'hidden',
    cursor: sortable ? 'pointer' : 'default',
  });
}

/**
 * A table header is `labelMedium` in the dim `text.primary` token (spec §2,
 * the same as MuiTableCell.head and the DataGrid header). The active sort
 * column, and a header under the pointer, read in the strong
 * `text.secondary` token instead. That colour step replaces the legacy
 * `opacity: 0.7` fade, which on top of the dim token would drop the header
 * under 4.5:1 in the light scheme.
 */
function headerTextSx(active: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    color: active ? theme.vars.palette.text.secondary : theme.vars.palette.text.primary,
    transition: 'color 0.2s ease',
    'th:hover > &': { color: theme.vars.palette.text.secondary },
    whiteSpace: 'nowrap',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
  });
}

