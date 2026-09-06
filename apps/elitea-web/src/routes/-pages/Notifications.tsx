/**
 * The page component for `/settings/notifications` (issue #493). See
 * `./PersonalTokens.tsx` for why this directory exists and why the page is
 * not declared inside its own route file.
 */
/**
 * ROUTE-062 `/settings/notifications` — notification center settings page.
 * Wires up `features/notifications` (list, bulk ops, item rendering).
 *
 * Adversarial-review fixes (cluster A11-api-model):
 *  - Scopes the list to the user's PERSONAL project (`personal_project_id`),
 *    not the currently-selected team/workspace project — matching
 *    `NotificationCenter.jsx:20,49` and `NotificationTable.jsx:64,141,146,
 *    148,167` (every baseline bulk action gates on / sends
 *    `personal_project_id` too). No shared accessor for it exists post-§2.3
 *    Redux removal, so this duplicates the local context-read shim
 *    `src/routes/_shell/settings/tokens.tsx` already carries for the same
 *    problem (`PersonalProjectIdContext`/`isPersonalProjectIdContext`/
 *    `selectPersonalProjectId` below) — same convention, independent copy.
 *  - Reaches pagination past page 0, a non-default page size, sorting, and
 *    search — all five already accepted by `ListNotificationsParams`
 *    (`features/notifications/api/notifications.ts`) but never supplied by
 *    this route. Ported control-for-control from `NotificationCenter.jsx`
 *    (search + pagination) and `NotificationTable.jsx` (sortable columns),
 *    minus the bespoke `entities/grid-table` widget itself — unported in
 *    this app (`*grid-table*` matches nothing under `src/**`) and too large
 *    a unit for this single-file cluster to build (FSD: a new shared
 *    entity, not a local adapter). Whoever picks that up should swap the
 *    `Select`-based sort control and `TablePagination` footer for the real
 *    grid; the `page`/`pageSize`/`sortBy`/`sortOrder`/`search` wiring into
 *    `useNotificationsList` does not need to change.
 *  - The bulk mark-toggle button flips between "Mark read"/"Mark unread"
 *    (sending the matching `isSeen`) based on whether the selection
 *    contains an unread row — `entities/notification`'s
 *    `hasUnreadAmongSelected` (ported for exactly this,
 *    `NotificationTable.jsx:135-143`) was exported but never called until
 *    now. That fix surfaced a companion bug blocking it entirely:
 *    `DrawerPageHeader` never got `showAddButton` (only `slotProps.
 *    addButton` was set, which no-ops without the flag), so "select all" —
 *    the ONLY selection mechanism here, no per-row checkboxes existing —
 *    never rendered and no bulk action was reachable, old code or new.
 *    Fixed here; it's inside this file and finding 3 isn't exercisable
 *    without it.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import MenuItem from '@mui/material/MenuItem';
import Paper from '@mui/material/Paper';
import Select from '@mui/material/Select';
import type { SelectChangeEvent } from '@mui/material/Select';
import TablePagination from '@mui/material/TablePagination';
import type { SxProps, Theme } from '@mui/material/styles';

import { useRouteContext } from '@tanstack/react-router';

import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import { t } from '@/shared/i18n';
import { RoutePending } from '@/routes/-ui/RouteStatus';
import { hasUnreadAmongSelected } from '@/entities/notification';
import { NotificationsListBody } from '@/features/notifications/ui/NotificationsListBody';
import {
  useBulkDeleteNotifications,
  useBulkMarkSeenNotifications,
  useNotificationsList,
} from '@/features/notifications/api/useNotifications';

/**
 * `personal_project_id` from the TanStack Router root context's
 * `auth.getUser()` (`src/app/router-context.ts`'s `AuthUser.
 * personal_project_id`) — outside this cluster's file scope, read
 * structurally rather than imported, per `no-upward-from-features`; the
 * same seam `src/routes/_shell/settings/tokens.tsx` and
 * `features/settings/ui/personal-tokens/TokensTable.tsx` each duplicate
 * independently already (see module doc comment above).
 */
interface PersonalProjectIdContext {
  readonly auth?: {
    readonly getUser?: () => { readonly personal_project_id?: string } | undefined;
  };
}

function isPersonalProjectIdContext(value: unknown): value is PersonalProjectIdContext {
  return typeof value === 'object' && value !== null;
}

function selectPersonalProjectId(context: unknown): string | undefined {
  if (!isPersonalProjectIdContext(context)) return undefined;
  return context.auth?.getUser?.()?.personal_project_id;
}

/**
 * Debounce a value — no shared debounce primitive exists yet in this app
 * (only local, per-call-site copies do: `src/pages/settings/Users.tsx`'s
 * own `useDebounce`). Delays the SEARCH QUERY PARAM only; the controlled
 * `<input>` (`DrawerPageHeader`'s `slotProps.searchInput`) still receives
 * every keystroke immediately, so typing itself never feels delayed.
 */
function useDebouncedValue<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(timer);
  }, [value, delayMs]);
  return debounced;
}

/**
 * `apps/elitea-ui/…/NotificationTable.jsx:30-34`'s two sortable columns
 * (`event_type`, `created_at`), collapsed into one control (see module doc
 * comment on why there is no per-column-header sort UI here). Values are
 * the literal `sort_by`/`sort_order` query params
 * (`features/notifications/api/notifications.ts`'s `buildNotificationsListUrl`).
 */
type NotificationSortBy = 'created_at' | 'event_type';
type NotificationSortOrder = 'asc' | 'desc';

interface SortOption {
  readonly value: string;
  readonly sortBy: NotificationSortBy;
  readonly sortOrder: NotificationSortOrder;
  readonly key: string;
  readonly fallback: string;
}

const SORT_OPTIONS: readonly SortOption[] = [
  { value: 'created_at:desc', sortBy: 'created_at', sortOrder: 'desc', key: 'routes.settings.notifications.sort.newest', fallback: 'Newest first' },
  { value: 'created_at:asc', sortBy: 'created_at', sortOrder: 'asc', key: 'routes.settings.notifications.sort.oldest', fallback: 'Oldest first' },
  { value: 'event_type:asc', sortBy: 'event_type', sortOrder: 'asc', key: 'routes.settings.notifications.sort.typeAsc', fallback: 'Type (A–Z)' },
  { value: 'event_type:desc', sortBy: 'event_type', sortOrder: 'desc', key: 'routes.settings.notifications.sort.typeDesc', fallback: 'Type (Z–A)' },
];

const PAGE_SIZE_OPTIONS = [10, 20, 50, 100] as const;
const DEFAULT_PAGE_SIZE = 20;
const SEARCH_DEBOUNCE_MS = 500;

/* ── small pure label helpers, split out to keep `NotificationsPage`'s own
   cyclomatic complexity under §3.5's budget (each ternary below would
   otherwise count against the component function itself) ────────────── */

function selectAllTooltip(selectedCount: number, rowCount: number): string {
  return selectedCount === rowCount
    ? t('routes.settings.notifications.deselectAll', 'Deselect all')
    : t('routes.settings.notifications.selectAll', 'Select all');
}

function markToggleLabel(shouldMarkAsRead: boolean): string {
  return shouldMarkAsRead
    ? t('routes.settings.notifications.markRead', 'Mark read')
    : t('routes.settings.notifications.markUnread', 'Mark unread');
}

function filterToggleLabel(filterNew: boolean): string {
  return filterNew
    ? t('routes.settings.notifications.showAll', 'Show all')
    : t('routes.settings.notifications.showNew', 'New only');
}

/** Exported (not just used via `Route`'s `component:`) so tests can mount it directly — same pattern as `src/routes/_shell/settings/tokens.tsx`'s `PersonalTokensPage`. */
export function NotificationsPage() {
  const routeContext: unknown = useRouteContext({ strict: false });
  const personalProjectId = selectPersonalProjectId(routeContext);

  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const [filterNew, setFilterNew] = useState(false);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState<number>(DEFAULT_PAGE_SIZE);
  const [sortValue, setSortValue] = useState<string>(SORT_OPTIONS[0]!.value);
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, SEARCH_DEBOUNCE_MS);

  const activeSort = useMemo(
    () => SORT_OPTIONS.find((option) => option.value === sortValue) ?? SORT_OPTIONS[0]!,
    [sortValue],
  );

  const { data, isFetching, isError, error } = useNotificationsList(
    {
      projectId: personalProjectId ?? '',
      page,
      pageSize,
      sortBy: activeSort.sortBy,
      sortOrder: activeSort.sortOrder,
      search: debouncedSearch,
      params: { only_new: filterNew },
    },
    { enabled: !!personalProjectId },
  );

  const bulkDelete = useBulkDeleteNotifications();
  const bulkMarkSeen = useBulkMarkSeenNotifications();

  const rows = useMemo(() => data?.rows ?? [], [data?.rows]);
  const total = data?.total ?? 0;
  const shouldMarkSelectionAsRead = useMemo(
    () => hasUnreadAmongSelected(rows, selectedIds),
    [rows, selectedIds],
  );
  const styles = getStyles();

  const handleSelectAll = useCallback(() => {
    setSelectedIds((prev) => (prev.size === rows.length ? new Set() : new Set(rows.map((r) => r.id))));
  }, [rows]);

  const handleDeleteSelected = useCallback(() => {
    if (selectedIds.size === 0 || !personalProjectId) return;
    bulkDelete.mutate({
      projectId: personalProjectId,
      ids: Array.from(selectedIds),
    });
    setSelectedIds(new Set());
  }, [selectedIds, personalProjectId, bulkDelete]);

  const handleMarkToggle = useCallback(() => {
    if (selectedIds.size === 0 || !personalProjectId) return;
    bulkMarkSeen.mutate({
      projectId: personalProjectId,
      ids: Array.from(selectedIds),
      isSeen: shouldMarkSelectionAsRead,
    });
    setSelectedIds(new Set());
  }, [selectedIds, personalProjectId, bulkMarkSeen, shouldMarkSelectionAsRead]);

  const handleToggleFilterNew = useCallback(() => {
    setFilterNew((p) => !p);
    setPage(0);
  }, []);

  const handleSearchChange = useCallback((value: string) => {
    setSearch(value);
    setPage(0);
  }, []);

  const handleSortChange = useCallback((event: SelectChangeEvent) => {
    setSortValue(event.target.value);
    setPage(0);
  }, []);

  const handlePageChange = useCallback((_event: unknown, nextPage: number) => {
    setPage(nextPage);
  }, []);

  const handlePageSizeChange = useCallback((event: { target: { value: string } }) => {
    setPageSize(Number(event.target.value));
    setPage(0);
  }, []);

  if (!personalProjectId) {
    return <RoutePending />;
  }

  return (
    <Paper elevation={0} sx={styles.root}>
      <DrawerPageHeader
        /* "Notifications Center", not "Notifications" — the drawer item is
           already called Notifications, and the page's own title in
           production names the screen. */
        title={t('routes.settings.notifications.title', 'Notifications Center')}
        showSearchInput
        showAddButton
        showBorder
        slotProps={{
          searchInput: {
            search,
            onChangeSearch: handleSearchChange,
            placeholder: t('routes.settings.notifications.searchPlaceholder', 'Search notifications…'),
          },
          addButton: {
            onAdd: handleSelectAll,
            tooltip: selectAllTooltip(selectedIds.size, rows.length),
          },
        }}
        extraContent={
          <Box sx={styles.actions}>
            <Select
              size="small"
              value={sortValue}
              onChange={handleSortChange}
              aria-label={t('routes.settings.notifications.sort.label', 'Sort by')}
              sx={styles.sortSelect}
            >
              {SORT_OPTIONS.map((option) => (
                <MenuItem key={option.value} value={option.value}>
                  {t(option.key, option.fallback)}
                </MenuItem>
              ))}
            </Select>
            <Button
              disabled={selectedIds.size === 0}
              onClick={handleMarkToggle}
            >
              {markToggleLabel(shouldMarkSelectionAsRead)}
            </Button>
            <Button
              color="error"
              disabled={selectedIds.size === 0}
              onClick={handleDeleteSelected}
            >
              {t('routes.settings.notifications.deleteSelected', 'Delete')}
            </Button>
            <Button onClick={handleToggleFilterNew}>
              {filterToggleLabel(filterNew)}
            </Button>
          </Box>
        }
      />
      <Box sx={styles.content}>
        <NotificationsListBody
          rows={rows}
          isFetching={isFetching}
          isError={isError}
          error={error}
          total={total}
          personalProjectId={personalProjectId}
        />
      </Box>
      {total > 0 && (
        <TablePagination
          component="div"
          count={total}
          page={page}
          rowsPerPage={pageSize}
          rowsPerPageOptions={[...PAGE_SIZE_OPTIONS]}
          onPageChange={handlePageChange}
          onRowsPerPageChange={handlePageSizeChange}
          labelRowsPerPage={t('routes.settings.notifications.pageSize', 'Rows per page')}
        />
      )}
    </Paper>
  );
}

const getStyles = (): {
  root: SxProps<Theme>;
  content: SxProps<Theme>;
  actions: SxProps<Theme>;
  sortSelect: SxProps<Theme>;
} => ({
  root: {
    display: 'flex',
    flexDirection: 'column',
    height: '100%',
    overflow: 'hidden',
    borderRadius: 'var(--el-shape-radiusSm, 0px)',
  },
  content: {
    flex: 1,
    minHeight: 0,
    overflow: 'auto',
    padding: '1rem 1.5rem',
  },
  actions: {
    display: 'flex',
    gap: '0.5rem',
    alignItems: 'center',
  },
  sortSelect: {
    minWidth: '9rem',
  },
});
