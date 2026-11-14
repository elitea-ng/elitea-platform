import { useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import { HeaderCell } from './EntityListHeaderCell';
import type { EntityListItem } from './model';
import { TableRow, checkCellSx } from './EntityListRow';
import { useEntityRowSlot } from './rowSlot';
import { useEntitySelection } from './selection';

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
  const slot = useEntityRowSlot();
  const selection = useEntitySelection();
  const showType = items.some((item) => item.typeLabel !== undefined && item.typeLabel !== '');
  const showStatus = items.some((item) => item.badge !== undefined);

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

  const columns = gridTemplateColumns(showType, slot !== undefined, selection !== undefined, showStatus);
  const ids = items.map((item) => item.id);
  const selectedCount = selection === undefined ? 0 : ids.filter((id) => selection.selected.has(id)).length;

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
          {selection !== undefined && (
            <Box
              component="th"
              scope="col"
              sx={checkCellSx}
            >
              <BaseCheckbox
                size="small"
                checked={ids.length > 0 && selectedCount === ids.length}
                indeterminate={selectedCount > 0 && selectedCount < ids.length}
                data-testid="entity-list-select-all"
                aria-label={t('shared.entityList.selectAll', 'Select all')}
                onChange={() => {
                  selection.toggleAll(ids);
                }}
              />
            </Box>
          )}
          <HeaderCell
            label={t('shared.entityList.table.name', 'Name & Description')}
            active={sortField === 'name'}
            descending={descending}
            onClick={() => {
              toggleSort('name');
            }}
          />
          {showStatus && <HeaderCell label={t('shared.entityList.table.status', 'Status')} />}
          {showType && <HeaderCell label={t('shared.entityList.table.type', 'Type')} />}
          <HeaderCell label={t('shared.entityList.table.authors', 'Authors')} />
          <HeaderCell
            label={t('shared.entityList.table.created', 'Created')}
            active={sortField === 'createdAt'}
            descending={descending}
            onClick={() => {
              toggleSort('createdAt');
            }}
          />
          {slot !== undefined && <HeaderCell label={slot.columnLabel} />}
        </Box>
      </Box>
      <Box
        component="tbody"
        sx={bodySx}
      >
        {sorted.map((item) => (
          <TableRow
            key={item.id}
            item={item}
            columns={columns}
            showType={showType}
            showStatus={showStatus}
            slot={slot}
          />
        ))}
      </Box>
    </Box>
  );
}

/** `DataTable.jsx`'s `gridTemplateColumns` — `minmax(<minWidth>, 1fr)` for the name column, fixed rem widths for the rest. */
function gridTemplateColumns(showType: boolean, showSlot: boolean, showSelect: boolean, showStatus: boolean): string {
  return [...(showSelect ? ['2.5rem'] : []), 'minmax(12.5rem, 1fr)', ...(showStatus ? ['6.5rem'] : []), ...(showType ? ['9.375rem'] : []), '15rem', '8.75rem', ...(showSlot ? ['11rem'] : [])].join(' ');
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

const bodySx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', width: '100%' };
