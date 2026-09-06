/**
 * NotificationsTable — Settings › Notifications, as a TABLE.
 *
 * Ported from `EliteaUI/src/pages/NotificationCenter/NotificationTable.jsx`
 * and the `entities/grid-table` primitives it composes
 * (`GridTableHeader`/`GridTableRow`/`GridTablePagination`).
 *
 * WHAT THIS REPLACES. This screen was a bare vertical list of
 * `NotificationListItem`s with no header row, no per-row checkbox, no
 * sorting, and a `TablePagination` footer. The route's own doc comment said
 * so and named the swap: "Whoever picks that up should swap the
 * `Select`-based sort control and `TablePagination` footer for the real
 * grid". Production is a four-column table — select-all checkbox, a sortable
 * Type icon column, the message, and a sortable right-aligned Date & Time —
 * over a footer reading "Rows per page: 50   1 - 1 of 1   ‹ ›".
 *
 * REAL TABLE ELEMENTS, LAID OUT WITH CSS GRID. The reference builds this from
 * `<div>`s carrying grid roles; that costs the semantics a `<table>` gives
 * for free, and `jsx-a11y(prefer-tag-over-role)` rejects it here. So the
 * markup is `table/thead/tbody/tr/th/td` and the LAYOUT is CSS: each `<tr>`
 * is a grid whose template is the reference's own `GRID_TEMPLATE_COLUMNS`,
 * `3rem 6rem minmax(0, 1fr) 11rem`. Measuring the live page confirms every
 * column boundary that template produces.
 */
import { memo } from 'react';

import Box from '@mui/material/Box';
import SvgIcon from '@mui/material/SvgIcon';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';
import { useTheme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { SortArrowsIcon } from '@/shared/ui/icons/sort-arrows-icon';

import type { NormalizedNotification } from '../api/normalize';
import type { NotificationSort, NotificationSortField } from '../lib/table';
import { formatNotificationTimestamp } from '../lib/timestamp';
import { NotificationIcon } from './NotificationIcon';
import { NotificationListItem } from './NotificationListItem';
import { NotificationsTablePagination } from './NotificationsTablePagination';

/** `NotificationTable.jsx:36`. Checkbox, type icon, message, timestamp. */
const GRID_TEMPLATE_COLUMNS = '3rem 6rem minmax(0, 1fr) 11rem';

export interface NotificationsTableProps {
  readonly rows: readonly NormalizedNotification[];
  readonly total: number;
  readonly page: number;
  readonly pageSize: number;
  readonly sort: NotificationSort;
  readonly selectedIds: ReadonlySet<string>;
  readonly personalProjectId: string;
  readonly onSort: (field: NotificationSortField) => void;
  readonly onSelectAll: () => void;
  readonly onSelectRow: (id: string) => void;
  readonly onPageChange: (page: number) => void;
  readonly onPageSizeChange: (pageSize: number) => void;
}

export const NotificationsTable = memo(function NotificationsTable(props: NotificationsTableProps) {
  const { rows, total, page, pageSize, sort, selectedIds, personalProjectId } = props;
  const { onSort, onSelectAll, onSelectRow, onPageChange, onPageSizeChange } = props;
  const theme = useTheme();

  const isAllSelected = rows.length > 0 && rows.every((row) => selectedIds.has(row.id));
  const isIndeterminate = !isAllSelected && rows.some((row) => selectedIds.has(row.id));

  return (
    <Box sx={wrapperSx} data-testid="notifications-table">
      <Box component="table" sx={tableSx}>
        <Box component="thead" sx={sectionSx}>
          <Box component="tr" sx={headerRowSx}>
            <Box component="th" scope="col" sx={headerCheckboxCellSx}>
              <BaseCheckbox
                checked={isAllSelected}
                indeterminate={isIndeterminate}
                onChange={onSelectAll}
                disabled={rows.length === 0}
                aria-label={t('routes.settings.notifications.selectAll', 'Select all')}
              />
            </Box>
            <SortableHeaderCell
              field="event_type"
              label={t('routes.settings.notifications.columnType', 'Type')}
              sort={sort}
              onSort={onSort}
            />
            <Box component="th" scope="col" sx={headerCellSx()}>
              <Typography variant="labelMedium" sx={headerTextSx}>
                {t('routes.settings.notifications.columnNotification', 'Notification')}
              </Typography>
            </Box>
            <SortableHeaderCell
              field="created_at"
              label={t('routes.settings.notifications.columnDate', 'Date & Time')}
              sort={sort}
              onSort={onSort}
              align="right"
            />
          </Box>
        </Box>

        <Box component="tbody" sx={sectionSx}>
          {rows.map((row) => (
            <Box
              component="tr"
              key={row.id}
              sx={bodyRowSx(selectedIds.has(row.id))}
              data-testid="notifications-table-row"
            >
              <Box component="td" sx={rowCheckboxCellSx}>
                <BaseCheckbox
                  checked={selectedIds.has(row.id)}
                  onChange={() => onSelectRow(row.id)}
                  aria-label={t('routes.settings.notifications.selectRow', 'Select notification')}
                />
              </Box>
              <Box component="td" sx={typeCellSx}>
                <NotificationIcon eventType={row.eventType} meta={row.meta} theme={theme} />
              </Box>
              <Box component="td" sx={messageCellSx}>
                <NotificationListItem
                  notification={row}
                  projectId={personalProjectId}
                  clampLines={0}
                  showTime={false}
                  showIcon={false}
                  context="table"
                  sx={listItemOverrideSx}
                />
              </Box>
              <Box component="td" sx={dateCellSx}>
                <Typography variant="labelMedium" sx={dateTextSx(row.isSeen)}>
                  {formatNotificationTimestamp(row.createdAt)}
                </Typography>
              </Box>
            </Box>
          ))}
        </Box>
      </Box>

      {total > 0 && (
        <NotificationsTablePagination
          page={page}
          pageSize={pageSize}
          total={total}
          onPageChange={onPageChange}
          onPageSizeChange={onPageSizeChange}
        />
      )}
    </Box>
  );
});

/**
 * `aria-sort` goes on the `<th>`; the click target is a real `<button>`
 * inside it. The reference hangs the handler on the header cell itself, which
 * leaves a `<div>` with no role and no tab stop — the column could be sorted
 * with a mouse and by no other means.
 */
function SortableHeaderCell({
  field,
  label,
  sort,
  onSort,
  align,
}: {
  field: NotificationSortField;
  label: string;
  sort: NotificationSort;
  onSort: (field: NotificationSortField) => void;
  align?: 'right';
}) {
  const isActive = sort.field === field;
  const ariaSort = isActive ? (sort.direction === 'asc' ? 'ascending' : 'descending') : 'none';

  return (
    <Box component="th" scope="col" aria-sort={ariaSort} sx={headerCellSx(align)}>
      <Box
        component="button"
        type="button"
        onClick={() => onSort(field)}
        sx={sortButtonSx(isActive)}
        aria-label={label}
      >
        <SvgIcon
          component={SortArrowsIcon}
          inheritViewBox
          sx={sortIconSx(isActive && sort.direction === 'desc')}
        />
      </Box>
      <Typography variant="labelMedium" sx={headerTextSx}>
        {label}
      </Typography>
    </Box>
  );
}

const wrapperSx: SxProps<Theme> = {
  width: '100%',
  flex: 1,
  display: 'flex',
  flexDirection: 'column',
  minHeight: 0,
};

/* `display: block`, not `table`: the rows below lay themselves out with CSS
 * grid, so the default table algorithm must not also try to size them. */
const tableSx: SxProps<Theme> = {
  display: 'block',
  width: '100%',
  flex: 1,
  minHeight: 0,
  overflow: 'auto',
  borderCollapse: 'collapse',
};

const sectionSx: SxProps<Theme> = { display: 'block', width: '100%' };

/* `GridTableHeader.jsx`: 2.25rem tall, its own panel background, a 1px rule
 * and an 8px radius. */
const headerRowSx: SxProps<Theme> = (theme) => ({
  display: 'grid',
  gridTemplateColumns: GRID_TEMPLATE_COLUMNS,
  alignItems: 'stretch',
  width: '100%',
  minHeight: '2.25rem',
  backgroundColor: theme.vars.palette.background.userInputBackground,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  borderRadius: theme.vars.shape.radiusMd,
  overflow: 'hidden',
});

const headerCheckboxCellSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  padding: '0.25rem',
};

const headerCellSx =
  (align?: 'right'): SxProps<Theme> =>
  () => ({
    display: 'flex',
    alignItems: 'center',
    gap: '0.25rem',
    padding: '0.25rem 0.75rem',
    minWidth: 0,
    overflow: 'hidden',
    ...(align === 'right' ? { justifyContent: 'flex-end' } : {}),
  });

const headerTextSx: SxProps<Theme> = (theme) => ({
  minWidth: 0,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
  color: theme.vars.palette.text.secondary,
});

/* `opacity: 0.7` until the column is the sorted one — the reference's
 * affordance for "sortable, but not the active sort". */
const sortButtonSx =
  (isActive: boolean): SxProps<Theme> =>
  (theme) => ({
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    padding: '0.375rem',
    border: 'none',
    background: 'transparent',
    cursor: 'pointer',
    borderRadius: theme.vars.shape.radiusPill,
    color: theme.vars.palette.text.primary,
    opacity: isActive ? 1 : 0.7,
    transition: 'opacity 0.2s ease',
    '&:hover': { opacity: 1 },
  });

const sortIconSx =
  (isDescending: boolean): SxProps<Theme> =>
  () => ({
    width: '1rem',
    height: '1rem',
    transform: isDescending ? 'rotate(180deg)' : 'rotate(0deg)',
    transition: 'transform 0.2s ease',
  });

const bodyRowSx =
  (isSelected: boolean): SxProps<Theme> =>
  (theme) => ({
    display: 'grid',
    gridTemplateColumns: GRID_TEMPLATE_COLUMNS,
    alignItems: 'center',
    width: '100%',
    minHeight: '2.5rem',
    borderBottom: `0.0625rem solid ${theme.vars.palette.border.table}`,
    backgroundColor: isSelected ? theme.vars.palette.background.userInputBackground : 'transparent',
    transition: 'background-color 0.2s ease',
    '&:hover': { backgroundColor: theme.vars.palette.background.userInputBackground },
    '&:last-of-type': { borderBottom: 'none' },
  });

const rowCheckboxCellSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'flex-start',
  justifyContent: 'center',
  alignSelf: 'start',
  paddingTop: '0.4rem',
  minWidth: 0,
};

const typeCellSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  alignSelf: 'start',
  paddingTop: '0.8rem',
  paddingLeft: '0.5rem',
  paddingRight: '0.5rem',
  '& > svg': { width: '1.125rem', height: '1.125rem', display: 'block' },
};

const messageCellSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'flex-start',
  alignSelf: 'start',
  width: '100%',
  minWidth: 0,
  padding: '0.5rem 0.8rem',
};

const listItemOverrideSx: SxProps<Theme> = {
  padding: 0,
  border: 'none',
  borderBottom: 'none',
  width: 'auto',
  flex: 1,
  minHeight: 'unset',
  minWidth: 0,
};

const dateCellSx: SxProps<Theme> = {
  display: 'flex',
  justifyContent: 'flex-end',
  alignSelf: 'start',
  padding: '0.5rem 0.8rem',
};

/* `NotificationTable.jsx:200-206`: a READ row's timestamp is `text.primary`
 * and an UNREAD one's is `text.secondary` — the brighter of the two — so the
 * date column carries the same unread signal the message does. */
const dateTextSx =
  (isSeen: boolean): SxProps<Theme> =>
  (theme) => ({
    paddingTop: '0.3rem',
    color: isSeen ? theme.vars.palette.text.primary : theme.vars.palette.text.secondary,
    whiteSpace: 'nowrap',
  });
