/**
 * State, data and write handlers for `pages/admin/Users.tsx` (unit A14).
 *
 * Split out of the page for the same reason `features/settings`'s users tab is
 * split: the page component stays a render, and the branching that decides
 * WHICH controls exist lives in one place instead of being spread across four
 * near-identical ternaries in JSX.
 *
 * The `undefined`-means-absent convention is deliberate: `AdminUsersTable`
 * renders no control at all for a handler it was not given, so "this tab has no
 * delete" and "this user may not delete" collapse into one representation and
 * cannot disagree with each other.
 */
import { useCallback, useMemo, useState } from 'react';

import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';

import type { AdminUserRowActions } from './AdminUsersTable';
import type { AdminReason } from './adminDangerousAction';
import { ADMIN_ACTION_PERMISSION, ADMIN_ITEM } from './adminItemIds';
import { adminUiShowsControlFor } from './adminUiConfig';
import { useAdminWritable } from './adminWriteAccess';
import { downloadCsv, fetchAllPages } from './adminCsv';
import { buildAdminUsersCsv } from './adminUsersCsv';
import { useAdminUserDangerousActions, type AdminUserDangerousDialog } from './useAdminUserDangerousActions';
import { useAdministrationRoles, type AdministrationRole } from './api/adminRoleOptionsApi';
import {
  fetchAdminUsersPage,
  useAdminUsers,
  type AdminUserRow,
  type AdminUsersPage,
  type AdminUserType,
} from './api/adminUsersApi';

/** Tab index → the `user_type` the server filters on. */
const USER_TYPES: readonly AdminUserType[] = ['platform', 'system'];
export const ADMIN_USERS_PAGE_SIZE = 20;


/** No roles while the roles API has not answered (the picker then lists what each person holds). */
const NO_ROLES: readonly AdministrationRole[] = [];

export interface AdminUsersPageState {
  readonly activeTab: number;
  readonly isSystemTab: boolean;
  readonly search: string;
  readonly page: number;
  readonly sortField: string;
  readonly sortDirection: 'asc' | 'desc';
  readonly selectedIds: number[];
  /** The dangerous action waiting for its confirmation (ADR-0059 R3.8b). */
  readonly dangerous: AdminUserDangerousDialog | undefined;
  readonly errorMessage: string;
  /** The user whose activity drawer is open, or `null` when it is closed. */
  readonly activityUser: AdminUserRow | null;

  readonly rows: readonly AdminUserRow[];
  readonly total: number;
  readonly counts: { readonly platform: number; readonly system: number };
  readonly isFetching: boolean;
  readonly isError: boolean;
  /** What the failed listing says: the server's refusal, or that it failed. */
  readonly loadErrorMessage: string;
  readonly isDeleting: boolean;
  readonly isExporting: boolean;
  readonly pendingIds: ReadonlySet<number>;

  /** The administration roles of the mode, from the roles API (ADR-0059 R0.8c). */
  readonly roleOptions: readonly AdministrationRole[];
  /** The roles list (needs `configuration.roles.permissions.view`) could not be read. */
  readonly rolesUnavailable: boolean;

  readonly onTabChange: (event: unknown, next: number) => void;
  readonly onSearchChange: (value: string) => void;
  readonly onSort: (field: string, direction: 'asc' | 'desc') => void;
  readonly onPreviousPage: () => void;
  readonly onNextPage: () => void;
  readonly onDismissError: () => void;
  readonly onRequestDelete: (ids: number[]) => void;
  readonly onCancelDangerous: () => void;
  readonly onConfirmDangerous: (reason: AdminReason) => void;
  /**
   * Activity is a READ over the audit endpoints, so it is not part of the
   * `undefined`-means-absent set below: it exists on both tabs and whatever the
   * admin-panel config advertises. The server authorises the read itself.
   */
  readonly onCloseActivity: () => void;
  /** Downloads every row the current tab + search select, as CSV. */
  readonly onExport: () => void;

  /** `undefined` ⇒ the control is not rendered on this tab / for this user. */
  readonly onSelectionChange: ((ids: number[]) => void) | undefined;
  /**
   * The per-row controls, as one MEMOISED object — `AdminUsersTable` is
   * `memo`'d and derives its columns from this, so a fresh literal per render
   * would defeat both. Grouped rather than passed flat because the table's
   * props reached the §3.5 budget of 12 when activity became live.
   */
  readonly rowActions: AdminUserRowActions;
}

/**
 * Extracted from the hook rather than inlined: the three in-flight checks push
 * `useAdminUsersPage` past the repo's complexity budget on their own, and they
 * are pure input → Set with nothing hook-shaped about them.
 */
/**
 * The three `data`-derived fields, defaulted in one place. Extracted for the
 * same reason as `collectPendingIds`: six `?.`/`??` links inline push the hook
 * past the complexity budget, and an un-resolved query defaulting to
 * "empty page, zero counts" is a fact about the RESPONSE, not about the hook.
 */
function readListing(data: AdminUsersPage | undefined): {
  rows: readonly AdminUserRow[];
  total: number;
  counts: { readonly platform: number; readonly system: number };
} {
  return {
    rows: data?.rows ?? [],
    total: data?.total ?? 0,
    counts: data?.counts ?? { platform: 0, system: 0 },
  };
}

/** A 403 is a refusal, not a failure: a narrow-role holder who types this URL is told so (R0.16). */
function listingErrorMessage(error: unknown): string {
  if (error instanceof EliteaApiError && error.failure.kind === 'http' && error.failure.status === 403) {
    return t('pages.admin.users.error.forbidden', 'You do not have the permission to see the users.');
  }
  return t('pages.admin.users.error.load', 'Failed to load users.');
}

interface InFlight<TVariables> {
  readonly isPending: boolean;
  readonly variables: TVariables | undefined;
}

function collectPendingIds(
  role: InFlight<{ userId: number }>,
  suspend: InFlight<{ userId: number }>,
  remove: InFlight<readonly number[]>,
): ReadonlySet<number> {
  const pending = new Set<number>();
  if (role.isPending && role.variables) pending.add(role.variables.userId);
  if (suspend.isPending && suspend.variables) pending.add(suspend.variables.userId);
  if (remove.isPending && remove.variables) remove.variables.forEach((id) => pending.add(id));
  return pending;
}

export function useAdminUsersPage(): AdminUsersPageState {
  const [activeTab, setActiveTab] = useState(0);
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(0);
  const [sortField, setSortField] = useState('name');
  const [sortDirection, setSortDirection] = useState<'asc' | 'desc'>('asc');
  const [selectedIds, setSelectedIds] = useState<number[]>([]);
  const [errorMessage, setErrorMessage] = useState('');
  const [activityUser, setActivityUser] = useState<AdminUserRow | null>(null);
  const [isExporting, setIsExporting] = useState(false);

  const isSystemTab = activeTab === 1;
  const userType = USER_TYPES[activeTab] ?? 'platform';

  // At `read` the controls are disabled (R3.4b, issue 2613), and the picker
  // lists only what each person holds.
  const access = useAdminWritable(ADMIN_ITEM.users);
  const rolesQuery = useAdministrationRoles({ enabled: access.writable && adminUiShowsControlFor(ADMIN_ACTION_PERMISSION.usersManage) });

  const listQuery = useAdminUsers({
    limit: ADMIN_USERS_PAGE_SIZE,
    offset: page * ADMIN_USERS_PAGE_SIZE,
    search: search || undefined,
    userType,
    sortBy: sortField,
    sortOrder: sortDirection,
  });

  const listing = useMemo(() => readListing(listQuery.data), [listQuery.data]);
  const onDeleted = useCallback((ids: readonly number[]) => {
    setSelectedIds((previous) => previous.filter((id) => !ids.includes(id)));
  }, []);
  const dangerous = useAdminUserDangerousActions({ rows: listing.rows, onError: setErrorMessage, onDeleted });
  const { deleteUsers, setAdminRole, suspendUser } = dangerous;

  /**
   * Rows with a mutation in flight. Read from react-query's `variables` (the
   * in-flight input) rather than tracked separately, so it cannot drift.
   */
  const pendingIds = useMemo(
    () =>
      collectPendingIds(
        { isPending: setAdminRole.isPending, variables: setAdminRole.variables },
        { isPending: suspendUser.isPending, variables: suspendUser.variables },
        { isPending: deleteUsers.isPending, variables: deleteUsers.variables?.userIds },
      ),
    [
    setAdminRole.isPending,
    setAdminRole.variables,
    suspendUser.isPending,
    suspendUser.variables,
      deleteUsers.isPending,
      deleteUsers.variables,
    ],
  );

  /**
   * A rejected write must SAY so. The reference page swallows every failure
   * ("Silent error handling"), which is how a 403 reads as "nothing happened".
   */
  const reportFailure = useCallback((fallback: string, error: unknown) => {
    setErrorMessage(error instanceof Error && error.message ? error.message : fallback);
  }, []);

  const onTabChange = useCallback((_event: unknown, next: number) => {
    setActiveTab(next);
    setPage(0);
    setSearch('');
    setSelectedIds([]);
    setErrorMessage('');
  }, []);

  const onSearchChange = useCallback((value: string) => {
    setSearch(value);
    setPage(0);
    setSelectedIds([]);
  }, []);

  const onSort = useCallback((field: string, direction: 'asc' | 'desc') => {
    setSortField(field);
    setSortDirection(direction);
    setPage(0);
  }, []);

  const onPreviousPage = useCallback(() => {
    setPage((previous) => Math.max(0, previous - 1));
    setSelectedIds([]);
  }, []);

  const onNextPage = useCallback(() => {
    setPage((previous) => previous + 1);
    setSelectedIds([]);
  }, []);

  const handleOpenActivity = useCallback((user: AdminUserRow) => setActivityUser(user), []);

  /**
   * Export. CSV rather than the reference's .xlsx — see `adminUsersCsv.ts` for
   * why the format differs. A failure is REPORTED, not swallowed: the reference
   * catches and discards it, so a 403 there looks like a click that did nothing.
   */
  const onExport = useCallback(() => {
    setErrorMessage('');
    setIsExporting(true);
    void (async () => {
      try {
        const { rows, truncated } = await fetchAllPages((limit, offset) =>
          fetchAdminUsersPage({
            limit,
            offset,
            search: search || undefined,
            userType,
            sortBy: sortField,
            sortOrder: sortDirection,
          }),
        );
        downloadCsv(`users-${userType}.csv`, buildAdminUsersCsv(rows));
        // A capped walk still downloads — but silently, a short file is
        // indistinguishable from a complete one, so it has to SAY so.
        if (truncated) {
          setErrorMessage(
            // `rows`, not `count`: i18next reads `count` as a plural selector
            // and would look for `_one`/`_other` keys this bundle has not got.
            t(
              'pages.admin.users.export.truncated',
              'The export was capped: the file holds the first {{rows}} users, not the whole list.',
              { rows: rows.length },
            ),
          );
        }
      } catch (error) {
        reportFailure(t('pages.admin.users.error.export', 'Failed to export the user list.'), error);
      } finally {
        setIsExporting(false);
      }
    })();
  }, [search, userType, sortField, sortDirection, reportFailure]);

  // System users are the platform's own service accounts: the legacy runtime
  // offered no role, suspend or delete control for them, and neither does this
  // port.
  const writable = !isSystemTab;

  const { onRequestRoles, onToggleSuspended, onRequestDelete } = dangerous;
  const rowActions = useMemo<AdminUserRowActions>(
    () => ({
      onSetAdminRoles: writable ? onRequestRoles : undefined,
      onToggleSuspended: writable ? onToggleSuspended : undefined,
      onDelete: writable ? onRequestDelete : undefined,
      // Not gated on `writable`: activity is a READ, so it exists on the
      // system tab and for an operator with no write permissions. The server
      // authorises the audit query itself.
      onOpenActivity: handleOpenActivity,
    }),
    [writable, onRequestRoles, onToggleSuspended, onRequestDelete, handleOpenActivity],
  );

  return {
    activeTab,
    isSystemTab,
    search,
    page,
    sortField,
    sortDirection,
    selectedIds: isSystemTab ? [] : selectedIds,
    dangerous: dangerous.dialog,
    errorMessage,
    activityUser,

    rows: listing.rows,
    total: listing.total,
    counts: listing.counts,
    isFetching: listQuery.isFetching,
    isError: listQuery.isError,
    loadErrorMessage: listingErrorMessage(listQuery.error),
    isDeleting: deleteUsers.isPending,
    isExporting,
    pendingIds,

    roleOptions: rolesQuery.data ?? NO_ROLES,
    rolesUnavailable: rolesQuery.isError,

    onTabChange,
    onSearchChange,
    onSort,
    onPreviousPage,
    onNextPage,
    onDismissError: useCallback(() => setErrorMessage(''), []),
    onRequestDelete,
    onCancelDangerous: dangerous.onCancel,
    onConfirmDangerous: dangerous.onConfirm,
    onCloseActivity: useCallback(() => setActivityUser(null), []),
    onExport,

    onSelectionChange: writable ? setSelectedIds : undefined,
    rowActions,
  };
}
