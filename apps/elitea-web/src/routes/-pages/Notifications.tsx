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
 *    this route.
 *
 * THE SCREEN IS A TABLE NOW. The note that used to sit here said the
 * `entities/grid-table` widget was unported, left a `Select`-based sort
 * control and a `TablePagination` footer as a stand-in, and asked whoever
 * picked it up to "swap [them] for the real grid". This is that swap.
 * `features/notifications/ui/NotificationsTable.tsx` carries the four-column
 * grid production draws — select-all checkbox, sortable Type, Notification,
 * sortable right-aligned Date & Time — over the reference's own pagination
 * footer. The `page`/`pageSize`/`sortBy`/`sortOrder`/`search` wiring into
 * `useNotificationsList` did not have to change, exactly as that note
 * predicted.
 *
 * Three controls went WITH the stand-in, because production's toolbar does
 * not have them: the "Newest first" sort dropdown (the column headers sort
 * now), the "New only" filter (the reference exposes no such toggle, even
 * though the `only_new` query param exists), and the round "+" button that
 * was doing select-all duty (the header checkbox does that). What is left is
 * what `NotificationTableToolbar.jsx` renders: a title, a search box, a
 * mark-read/unread button and a delete button, the last two disabled until
 * something is selected.
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
import Paper from '@mui/material/Paper';
import type { SxProps, Theme } from '@mui/material/styles';

import { useRouteContext } from '@tanstack/react-router';

import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import { t } from '@/shared/i18n';
import { RoutePending } from '@/routes/-ui/RouteStatus';
import { hasUnreadAmongSelected } from '@/entities/notification';
import { NotificationsListBody } from '@/features/notifications/ui/NotificationsListBody';
import { NotificationsTable } from '@/features/notifications/ui/NotificationsTable';
import { NotificationsToast, type NotificationToastKind } from '@/features/notifications/ui/NotificationsToast';
import {
  NOTIFICATION_DEFAULT_PAGE_SIZE,
  NOTIFICATION_DEFAULT_SORT,
  type NotificationSort,
  type NotificationSortField,
  apiSearchTerm,
  nextSort,
} from '@/features/notifications/lib/table';
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
 * The reference debounces search by 600ms (`NotificationCenter.jsx:34`) and
 * ignores anything shorter than two characters (`apiSearchTerm`).
 */
const SEARCH_DEBOUNCE_MS = 600;

function markToggleLabel(shouldMarkAsRead: boolean): string {
  return shouldMarkAsRead
    ? t('routes.settings.notifications.markRead', 'Mark selected as read')
    : t('routes.settings.notifications.markUnread', 'Mark selected as unread');
}

/** Exported (not just used via `Route`'s `component:`) so tests can mount it directly — same pattern as `src/routes/_shell/settings/tokens.tsx`'s `PersonalTokensPage`. */
export function NotificationsPage() {
  const routeContext: unknown = useRouteContext({ strict: false });
  const personalProjectId = selectPersonalProjectId(routeContext);

  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState<number>(NOTIFICATION_DEFAULT_PAGE_SIZE);
  const [sort, setSort] = useState<NotificationSort>(NOTIFICATION_DEFAULT_SORT);
  const [search, setSearch] = useState('');
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [toast, setToast] = useState<NotificationToastKind>();
  const debouncedSearch = useDebouncedValue(search, SEARCH_DEBOUNCE_MS);

  const { data, isFetching, isError, error } = useNotificationsList(
    {
      projectId: personalProjectId ?? '',
      page,
      pageSize,
      sortBy: sort.field,
      sortOrder: sort.direction,
      search: apiSearchTerm(debouncedSearch),
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

  /*
   * A bulk delete asks first, and BOTH bulk actions report their result.
   *
   * The reference confirms the delete through `DeleteEntityButton`
   * (`NotificationTableToolbar.jsx:63-73`, `shouldRequestInputName={false}`)
   * and toasts every outcome of both actions
   * (`NotificationTable.jsx:145-185`). This page deleted on the first click
   * and dropped every result: a refused mutation looked exactly like a
   * successful one, because the selection was cleared before the request
   * even resolved (issue 841).
   *
   * The selection is cleared in `onSuccess` for the same reason. A failed
   * delete that clears the selection tells the reader the rows are gone.
   */
  const handleConfirmDelete = useCallback(() => {
    if (selectedIds.size === 0 || !personalProjectId) return;
    setDeleteOpen(false);
    bulkDelete.mutate(
      { projectId: personalProjectId, ids: Array.from(selectedIds) },
      {
        onSuccess: () => {
          setSelectedIds(new Set());
          setToast('deleted');
        },
        onError: () => setToast('error'),
      },
    );
  }, [selectedIds, personalProjectId, bulkDelete]);

  const handleMarkToggle = useCallback(() => {
    if (selectedIds.size === 0 || !personalProjectId) return;
    const markAsRead = shouldMarkSelectionAsRead;
    bulkMarkSeen.mutate(
      { projectId: personalProjectId, ids: Array.from(selectedIds), isSeen: markAsRead },
      {
        onSuccess: () => {
          setSelectedIds(new Set());
          setToast(markAsRead ? 'read' : 'unread');
        },
        onError: () => setToast('error'),
      },
    );
  }, [selectedIds, personalProjectId, bulkMarkSeen, shouldMarkSelectionAsRead]);

  const handleSearchChange = useCallback((value: string) => {
    setSearch(value);
    setPage(0);
  }, []);

  /*
   * A sort change goes back to page 0. Staying on page 4 of a re-sorted list
   * shows a window into rows the reader never asked about, and if the new
   * ordering has fewer pages it shows nothing at all.
   */
  const handleSort = useCallback((field: NotificationSortField) => {
    setSort((previous) => nextSort(previous, field));
    setPage(0);
  }, []);

  const handleSelectRow = useCallback((id: string) => {
    setSelectedIds((previous) => {
      const next = new Set(previous);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const handlePageChange = useCallback((nextPage: number) => {
    setPage(nextPage);
  }, []);

  const handlePageSizeChange = useCallback((size: number) => {
    setPageSize(size);
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
        showBorder
        slotProps={{
          searchInput: {
            search,
            onChangeSearch: handleSearchChange,
            placeholder: t('routes.settings.notifications.searchPlaceholder', 'Search'),
          },
        }}
        extraContent={
          <Box sx={styles.actions}>
            <Button
              variant="secondary"
              data-testid="notification-mark-toggle-button"
              disabled={selectedIds.size === 0}
              onClick={handleMarkToggle}
            >
              {markToggleLabel(shouldMarkSelectionAsRead)}
            </Button>
            <Button
              variant="secondary"
              color="alarm"
              data-testid="notification-delete-button"
              disabled={selectedIds.size === 0}
              onClick={() => setDeleteOpen(true)}
            >
              {t('routes.settings.notifications.deleteSelected', 'Delete')}
            </Button>
          </Box>
        }
      />
      <Box sx={styles.content}>
        {/* The list body is still the authority on the three NON-row states
          * — failed read, first load, empty list — and it must stay ahead of
          * the table: a failed read leaves `rows` empty and `isFetching`
          * false, so rendering the grid unconditionally would draw an empty
          * inbox over a 404 (issue 413). Only a resolved, non-empty read
          * reaches the table. */}
        {isError || rows.length === 0 ? (
          <NotificationsListBody
            rows={rows}
            isFetching={isFetching}
            isError={isError}
            error={error}
            total={total}
            personalProjectId={personalProjectId}
          />
        ) : (
          <NotificationsTable
            rows={rows}
            total={total}
            page={page}
            pageSize={pageSize}
            sort={sort}
            selectedIds={selectedIds}
            personalProjectId={personalProjectId}
            onSort={handleSort}
            onSelectAll={handleSelectAll}
            onSelectRow={handleSelectRow}
            onPageChange={handlePageChange}
            onPageSizeChange={handlePageSizeChange}
          />
        )}
      </Box>
      <DeleteEntityModal
        open={deleteOpen}
        onClose={() => setDeleteOpen(false)}
        onConfirm={handleConfirmDelete}
        confirming={bulkDelete.isPending}
        shouldRequestInputName={false}
        name={t('routes.settings.notifications.deleteEntityName', 'selected notifications')}
        copy={{ title: t('routes.settings.notifications.deleteTitle', 'Delete selected notifications') }}
      />
      <NotificationsToast
        kind={toast}
        onClose={() => setToast(undefined)}
      />
    </Paper>
  );
}

const getStyles = (): {
  root: SxProps<Theme>;
  content: SxProps<Theme>;
  actions: SxProps<Theme>;
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
    display: 'flex',
    flexDirection: 'column',
    overflow: 'auto',
    padding: '1rem 1.5rem',
  },
  /* `NotificationTableToolbar.jsx`'s `rightSection`: `0.6rem` between the
   * two action buttons. */
  actions: {
    display: 'flex',
    gap: '0.6rem',
    alignItems: 'center',
  },
});
