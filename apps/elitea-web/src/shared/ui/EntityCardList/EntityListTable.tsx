import { useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { CalendarIcon } from '@/shared/ui/icons/calendar-icon';

import { EntityCardAuthors } from './EntityCardAuthors';
import type { EntityListItem } from './model';

/**
 * The `?view=table` rendering of a list page — the port of
 * `apps/elitea-ui/src/[fsd]/widgets/data-table/ui/DataTable.jsx` over
 * `[fsd]/entities/grid-table/ui/{GridTableHeader,GridTableRow}`.
 *
 * Geometry from the baseline: a `2.25rem` header bar on
 * `background.userInputBackground` with a `border.lines` outline and a
 * `0.5rem` radius, `3.5rem` rows separated by `border.table`, a
 * `0.375rem 1rem` cell padding, and the row hover/selected background on
 * `background.userInputBackground`.
 *
 * DISCLOSED NARROWING vs. the baseline's `DataTable`: no per-row checkbox,
 * no `StatusBar`/`Like`/`Rate` column, no kebab actions cell, and no
 * server-side sort round-trip — the header sorts the rows this component
 * was handed. Those all hang off cross-cutting widgets (`widgets/pin-
 * toggler`, `Like`, `DataTableActionsCell`) with no port in this app.
 */
type SortField = 'name' | 'createdAt';

export interface EntityListTableProps {
  readonly items: readonly EntityListItem[];
}

export function EntityListTable({ items }: EntityListTableProps): ReactNode {
  const [sortField, setSortField] = useState<SortField>('createdAt');
  const [descending, setDescending] = useState(true);
  const showType = items.some((item) => item.typeLabel !== undefined && item.typeLabel !== '');

  const sorted = useMemo(() => {
    const direction = descending ? -1 : 1;
    return [...items].sort((left, right) => {
      const a = sortField === 'name' ? left.name : (left.createdAt ?? '');
      const b = sortField === 'name' ? right.name : (right.createdAt ?? '');
      return a.localeCompare(b) * direction;
    });
  }, [items, sortField, descending]);

  const toggleSort = (field: SortField): void => {
    if (field === sortField) {
      setDescending((previous) => !previous);
      return;
    }
    setSortField(field);
    setDescending(true);
  };

  const columns = gridTemplateColumns(showType);

  return (
    <Box
      component="table"
      sx={tableSx}
    >
      <Box component="thead">
        <Box
          component="tr"
          sx={headerSx(columns)}
        >
          <HeaderCell
            label={t('shared.entityList.table.name', 'Name & Description')}
            active={sortField === 'name'}
            onClick={() => {
              toggleSort('name');
            }}
          />
          {showType && <HeaderCell label={t('shared.entityList.table.type', 'Type')} />}
          <HeaderCell label={t('shared.entityList.table.authors', 'Authors')} />
          <HeaderCell
            label={t('shared.entityList.table.created', 'Created')}
            active={sortField === 'createdAt'}
            onClick={() => {
              toggleSort('createdAt');
            }}
          />
        </Box>
      </Box>
      <Box
        component="tbody"
        sx={bodySx}
      >
        {sorted.map((item) => (
          <Box
            component="tr"
            key={item.id}
            data-testid="entity-list-row"
            sx={rowSx(columns, item.onClick !== undefined)}
            onClick={() => {
              item.onClick?.();
            }}
          >
            <Box
              component="td"
              sx={nameCellSx}
            >
              <Box sx={rowIconSx}>{item.icon}</Box>
              <Box sx={nameTextSx}>
                <Typography
                  variant="bodyMedium"
                  sx={rowNameSx}
                >
                  {item.name}
                </Typography>
                {item.description !== undefined && item.description !== '' && (
                  <Typography
                    variant="bodySmall"
                    sx={rowDescriptionSx}
                  >
                    {item.description}
                  </Typography>
                )}
              </Box>
            </Box>
            {showType && (
              <Box
                component="td"
                sx={cellSx}
              >
                <Typography
                  variant="bodyMedium"
                  sx={clampSx}
                >
                  {item.typeLabel ?? ''}
                </Typography>
              </Box>
            )}
            <Box
              component="td"
              sx={cellSx}
            >
              <EntityCardAuthors authors={item.authors ?? []} />
            </Box>
            <Box
              component="td"
              sx={createdCellSx}
            >
              {item.createdAt !== undefined && (
                <>
                  <Box
                    component={CalendarIcon}
                    sx={calendarIconSx}
                  />
                  <Typography
                    variant="bodyMedium"
                    component="span"
                  >
                    {formatCreated(item.createdAt)}
                  </Typography>
                </>
              )}
            </Box>
          </Box>
        ))}
      </Box>
    </Box>
  );
}

/** `TIME_FORMAT.MMMDD` (`apps/elitea-ui/src/common/constants.js`) — "Mar 14". */
function formatCreated(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  return date.toLocaleDateString(undefined, { month: 'short', day: '2-digit' });
}

interface HeaderCellProps {
  readonly label: string;
  readonly active?: boolean;
  readonly onClick?: () => void;
}

function HeaderCell({ label, active = false, onClick }: HeaderCellProps): ReactNode {
  return (
    <Box
      component="th"
      scope="col"
      sx={headerCellSx(active, onClick !== undefined)}
      onClick={onClick}
    >
      <Typography
        variant="labelMedium"
        sx={headerTextSx}
      >
        {label}
      </Typography>
    </Box>
  );
}

/** `DataTable.jsx`'s `gridTemplateColumns` — `minmax(<minWidth>, 1fr)` for the name column, fixed rem widths for the rest. */
function gridTemplateColumns(showType: boolean): string {
  return ['minmax(12.5rem, 1fr)', ...(showType ? ['9.375rem'] : []), '15rem', '8.75rem'].join(' ');
}

const tableSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.75rem',
  minWidth: '42.5rem',
  width: '100%',
};

function headerSx(columns: string): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'grid',
    gridTemplateColumns: columns,
    alignItems: 'stretch',
    width: '100%',
    height: '2.25rem',
    backgroundColor: theme.vars.palette.background.userInputBackground,
    border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
    borderRadius: theme.vars.shape.radiusMd,
    overflow: 'hidden',
  });
}

function headerCellSx(active: boolean, sortable: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    gap: theme.spacing(0.5),
    padding: `${theme.spacing(0.5)} ${theme.spacing(1.5)}`,
    minWidth: 0,
    overflow: 'hidden',
    cursor: sortable ? 'pointer' : 'default',
    opacity: active ? 1 : 0.7,
    transition: 'opacity 0.2s ease',
    '&:hover': { opacity: 1 },
  });
}

const headerTextSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.secondary,
  whiteSpace: 'nowrap',
  overflow: 'hidden',
  textOverflow: 'ellipsis',
});

const bodySx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', width: '100%' };

function rowSx(columns: string, clickable: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'grid',
    gridTemplateColumns: columns,
    alignItems: 'center',
    width: '100%',
    height: '3.5rem',
    minHeight: '3.5rem',
    flexShrink: 0,
    cursor: clickable ? 'pointer' : 'default',
    borderBottom: `0.0625rem solid ${theme.vars.palette.border.table}`,
    transition: 'background-color 0.2s ease',
    '&:hover': { backgroundColor: theme.vars.palette.background.userInputBackground },
    '&:last-of-type': { borderBottom: 'none' },
  });
}

const cellSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  padding: `${theme.spacing(0.75)} ${theme.spacing(2)}`,
  minWidth: 0,
  overflow: 'hidden',
});

const nameCellSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1.5),
  padding: `${theme.spacing(0.75)} ${theme.spacing(2)}`,
  minWidth: 0,
  overflow: 'hidden',
});

const rowIconSx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  width: '1.75rem',
  height: '1.75rem',
  borderRadius: theme.vars.shape.radiusPill,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  background: theme.vars.palette.background.icon.entityGradient,
  color: theme.vars.palette.text.primary,
  '& > svg': { width: '0.875rem', height: '0.875rem' },
});

const nameTextSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', minWidth: 0 };

const clampSx: SxProps<Theme> = {
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const rowNameSx: SxProps<Theme> = (theme: Theme) => ({ ...clampSx, color: theme.vars.palette.text.secondary });

const rowDescriptionSx: SxProps<Theme> = (theme: Theme) => ({ ...clampSx, color: theme.vars.palette.text.primary });

const createdCellSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  gap: theme.spacing(1),
  padding: `${theme.spacing(0.75)} ${theme.spacing(2)}`,
  minWidth: 0,
});

const calendarIconSx: SxProps<Theme> = { width: '1rem', height: '1rem', flexShrink: 0 };
