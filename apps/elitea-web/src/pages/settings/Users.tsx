// @ts-nocheck
/**
 * Users — settings page that wires data hooks, actions, and rendering state
 * to the `UsersPageContent` shell. Ported from
 * `apps/elitea-ui/src/[fsd]/pages/settings/Users.jsx`.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import { usePermissionList } from '@/shared/api/generated/auth/auth';
import { useUserList, useRoleList } from '@/shared/api/generated/admin/admin';
import type { UserRecord } from '@/shared/api/generated/model';
import { unwrapList, unwrapListPage } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { usePermissionSet } from '@/widgets/sidebar';
import { usersFeature } from '@/features/settings';
import { errorMessage, inviteSuccessMessage, invitePartialMessage } from './usersToast';
import { useInviteDialog } from './useInviteDialog';

const { useUsersActions, UsersPageContent } = usersFeature;

const ROWS_PER_PAGE_DEFAULT = 20;

/* ── helpers ───────────────────────────────────────────────────────────── */

/** Debounce value and return { value, isDebounce }. */
function useDebounce<T>(value: T, delayMs: number): { value: T; isDebounce: boolean } {
  const [debounced, setDebounced] = useState(value);
  const [isDebounce, setIsDebounce] = useState(true);

  useEffect(() => {
    const timer = setTimeout(() => {
      setDebounced(value);
      setIsDebounce(false);
    }, delayMs);
    return () => clearTimeout(timer);
  }, [value, delayMs]);

  return { value: debounced, isDebounce };
}


/* ── custom hook: useUsersPageData ────────────────────────────────────── */

interface UseUsersPageDataResult {
  rawUsers: UserRecord[];
  rolesOptions: Array<{ label: string; value: string }>;
  filteredUsers: UserRecord[];
  pagedUsers: UserRecord[];
  debouncedSearch: string;
  page: number;
  setPage: (p: number) => void;
  loadedPage: number;
  setLoadedPage: (p: number) => void;
  pageSize: number;
  setPageSize: (s: number) => void;
  serverTotal: number;
  selectedUsers: UserRecord[];
  setSelectedUsers: (u: UserRecord[] | ((prev: UserRecord[]) => UserRecord[])) => void;
  sortField: string;
  setSortField: (f: string) => void;
  sortDirection: 'asc' | 'desc';
  setSortDirection: (d: 'asc' | 'desc') => void;
  searchText: string;
  setSearchText: (s: string) => void;
  // `isError` is part of the shape on purpose. The returned literals used to
  // narrow to `{isFetching, refetch}`. That narrowing made a failed member
  // fetch unreachable from the page. The table rendered its "No users"
  // placeholder, and no error was shown anywhere.
  userListQuery: { isFetching: boolean; isError: boolean; refetch?: () => void };
  roleListQuery: { isFetching: boolean; isError: boolean; refetch?: () => void };
}

/**
 * Server-side-pagination-aware data layer (old-app parity: `Users.jsx`'s
 * `useUserListQuery` + `onChangePage`'s incremental `setPage`/RTK-Query
 * `merge`). `useUserList` fetches one `{limit, offset}` window per call, so
 * this hook accumulates pages itself: page 0 REPLACES, any later page
 * UPSERTS — refreshing rows an edit/delete's refetch touches instead of
 * silently going stale.
 */
function useUsersPageData(projectId: string, canView: boolean): UseUsersPageDataResult {
  const [page, setPage] = useState(0);
  const [loadedPage, setLoadedPage] = useState(0);
  const [pageSize, setPageSize] = useState(ROWS_PER_PAGE_DEFAULT);
  const [selectedUsers, setSelectedUsers] = useState<UserRecord[]>([]);
  const [sortField, setSortField] = useState('name');
  const [sortDirection, setSortDirection] = useState<'asc' | 'desc'>('asc');
  const [searchText, setSearchText] = useState('');
  const { value: debouncedSearch } = useDebounce(searchText, 300);
  const [accumulatedUsers, setAccumulatedUsers] = useState<UserRecord[]>([]);
  const [serverTotal, setServerTotal] = useState(0);

  const userListQuery = useUserList(
    projectId,
    { limit: pageSize, offset: loadedPage * pageSize },
    { query: { enabled: !!projectId && canView } },
  ) as { isFetching: boolean; isError: boolean; refetch?: () => void; data?: unknown };

  const roleListQuery = useRoleList(
    projectId,
    { limit: 1000, offset: 0 },
    { query: { enabled: !!projectId && canView } },
  ) as { isFetching: boolean; isError: boolean; refetch?: () => void; data?: unknown };

  useEffect(() => {
    const resp = userListQuery.data;
    if (!resp) return;
    // The original `resp.data.data.rows` went one level too deep (`eliteaFetch`
    // already unwraps the transport into {data,status,headers}), resolved to
    // undefined, and made `rows` permanently [] — every user saw an empty
    // members table with no error anywhere. Measured body: {"rows":[…],"total":2}.
    // unwrapListPage takes the envelope OR the body, so "one level too deep" and
    // "one level too shallow" both stop being expressible here (R-A6, #132).
    const { rows, total } = unwrapListPage<UserRecord>(resp, 'userList');
    setServerTotal(total);
    setAccumulatedUsers((prev) => {
      if (loadedPage === 0) return rows;
      const byId = new Map(prev.map((u) => [u.id, u] as const));
      rows.forEach((row) => byId.set(row.id, row));
      return Array.from(byId.values());
    });
  }, [userListQuery.data, loadedPage]);

  const rawUsers = accumulatedUsers;

  const rawRoles = useMemo(() => {
    const resp = roleListQuery.data;
    if (!resp) return [] as { id: string; name: string }[];
    // Roles differs in shape from users: the body is a BARE ARRAY, not
    // {rows,total}. Measured: [{"id":"1","name":"admin"},{"id":"2",…}].
    // The old `resp.data.data.rows` was wrong twice over — too deep AND reading
    // a `rows` key that does not exist here — so `rolesOptions` was always [],
    // the role SingleSelect showed only its disabled "No options" entry, and
    // because Invite is gated on a selected role the invite flow could never be
    // completed by anyone. This is the more serious half of the bug.
    //
    // Note the call is IDENTICAL to the users one above even though the two
    // bodies differ; that is the point of the helper — the per-endpoint shape
    // stops being something a call site has to know or copy correctly.
    return unwrapList<{ id: string; name: string }>(resp, 'roleList');
  }, [roleListQuery.data]);

  const rolesOptions = useMemo(
    () => rawRoles.map((r) => ({ label: r.name, value: r.name })),
    [rawRoles],
  );

  const filteredUsers = useMemo(() => {
    if (!debouncedSearch) return rawUsers;
    const query = debouncedSearch.toLowerCase();
    return rawUsers.filter(
      (user) =>
        user.name.toLowerCase().includes(query) ||
        user.email.toLowerCase().includes(query) ||
        user.roles.some((r) => r.toLowerCase().includes(query)),
    );
  }, [debouncedSearch, rawUsers]);

  const sortedUsers = useMemo(() => {
    const sorted = [...filteredUsers];
    sorted.sort((a, b) => {
      let cmp = 0;
      if (sortField === 'name') cmp = a.name.localeCompare(b.name);
      else if (sortField === 'email') cmp = a.email.localeCompare(b.email);
      return sortDirection === 'asc' ? cmp : -cmp;
    });
    return sorted;
  }, [filteredUsers, sortField, sortDirection]);

  const pagedUsers = useMemo(
    () => sortedUsers.slice(page * pageSize, page * pageSize + pageSize),
    [sortedUsers, page, pageSize],
  );

  return {
    rawUsers,
    rolesOptions,
    filteredUsers,
    pagedUsers,
    debouncedSearch,
    page,
    setPage,
    loadedPage,
    setLoadedPage,
    pageSize,
    setPageSize,
    serverTotal,
    selectedUsers,
    setSelectedUsers,
    sortField,
    setSortField,
    sortDirection,
    setSortDirection,
    searchText,
    setSearchText,
    userListQuery: { isFetching: userListQuery.isFetching, isError: userListQuery.isError, refetch: userListQuery.refetch },
    roleListQuery: { isFetching: roleListQuery.isFetching, isError: roleListQuery.isError, refetch: roleListQuery.refetch },
  };
}

/* ── component ─────────────────────────────────────────────────────────── */

export interface UsersProps {
  projectId: string;
}

export function Users({ projectId }: UsersProps) {
  // ── permissions (spec §9.3, old-app parity: `checkPermission(PERMISSIONS.users.*)`) ──
  const permissionSet = usePermissionSet(projectId || undefined);
  // `usePermissionSet` answers an EMPTY set while the permission request is
  // still in flight, so `canView === false` alone cannot tell "denied" from
  // "not known yet". This reads the same query purely for its settled state,
  // so the no-permission banner does not flash for everyone. React-query
  // serves that query from one cache entry, so it costs no extra request.
  const permissionQuery = usePermissionList(projectId || '', { query: { enabled: !!projectId } });
  const permissionsResolved = !!projectId && (permissionQuery.isSuccess || permissionQuery.isError);
  const canView = permissionSet.has(PERMISSIONS.users.view);
  const canCreate = permissionSet.has(PERMISSIONS.users.create);
  const canEdit = permissionSet.has(PERMISSIONS.users.edit);
  const canDelete = permissionSet.has(PERMISSIONS.users.delete);

  // ── extracted data ───────────────────────────────────────────────────
  const pageData = useUsersPageData(projectId, canView);
  const {
    rawUsers, rolesOptions, filteredUsers, pagedUsers, debouncedSearch,
    page, setPage, setLoadedPage, pageSize, setPageSize, serverTotal,
    selectedUsers, setSelectedUsers, sortField, setSortField,
    sortDirection, setSortDirection, searchText, setSearchText,
    userListQuery, roleListQuery,
  } = pageData;

  // ── local state (toast + invite) ─────────────────────────────────────
  const invite = useInviteDialog();
  const [toastMessage, setToastMessage] = useState('');
  const [toastType, setToastType] = useState<'success' | 'error'>('success');

  // ── callbacks ────────────────────────────────────────────────────────
  const handleSearchChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    setSearchText(e.target.value);
    setPage(0);
    setLoadedPage(0);
  }, [setSearchText, setPage, setLoadedPage]);

  const handlePageSizeChange = useCallback((size: number) => {
    setPageSize(size);
    setPage(0);
    setLoadedPage(0);
    setSelectedUsers([]);
  }, [setPageSize, setPage, setLoadedPage, setSelectedUsers]);

  // Old-app parity (`Users.jsx`'s `onChangePage`): only fetch more when loaded rows don't cover the requested page.
  const handleChangePage = useCallback((newPage: number) => {
    if (newPage < 0) return;
    const loadLimit = (newPage + 1) * pageSize;
    if (filteredUsers.length !== serverTotal && filteredUsers.length < loadLimit) {
      setLoadedPage(newPage);
    }
    setPage(newPage);
  }, [pageSize, serverTotal, filteredUsers.length, setLoadedPage, setPage]);

  const handleSort = useCallback((field: string, direction: 'asc' | 'desc') => {
    setSortField(field);
    setSortDirection(direction);
  }, [setSortField, setSortDirection]);

  const handleSelectPage = useCallback((selected: boolean) => {
    setSelectedUsers(selected ? [...pagedUsers] : []);
  }, [pagedUsers, setSelectedUsers]);

  const handleSelectRow = useCallback((user: { id: string }, selected: boolean) => {
    const fullUser = rawUsers.find((u) => u.id === user.id);
    if (!fullUser) return;
    setSelectedUsers((prev) => (selected ? [...prev, fullUser] : prev.filter((u) => u.id !== user.id)));
  }, [rawUsers, setSelectedUsers]);

  // ── actions ──────────────────────────────────────────────────────────
  const actionsResult = useUsersActions({
    projectId,
    selectedUsers,
    rolesOptions,
    onDeleteSuccess: () => {
      const wasMultiple = selectedUsers.length > 1;
      setSelectedUsers([]);
      setToastType('success');
      setToastMessage(
        wasMultiple
          ? t('shared.ui.settings.users.multipleUsersDeleted', 'The users have been deleted')
          : t('shared.ui.settings.users.userDeleted', 'The user has been deleted'),
      );
      userListQuery.refetch?.();
    },
    onDeleteError: (error: unknown) => {
      setToastType('error');
      setToastMessage(errorMessage(error));
    },
    onInviteSuccess: (outcome) => {
      // Every address landed, so there is nothing on the per-address list an
      // operator has to read: close, and say it in one line.
      invite.close();
      setToastType('success');
      // "Invited" only when an invitation e-mail actually went out; a
      // deployment without outbound e-mail has ADDED the user (they sign in
      // on their own) and the toast must not claim otherwise (ADR-0024 WP7).
      setToastMessage(inviteSuccessMessage(outcome.summary));
      userListQuery.refetch?.();
    },
    onInviteError: (error: unknown, outcome) => {
      // A 400 with rows is a PARTIAL result, not a rejected request: the `ok`
      // rows in that body have already been written, each in its own
      // transaction. Keep the dialog open and hand it the rows, so the operator
      // reads WHICH address failed and why — the single red toast this replaces
      // could not tell "one already a member" from "all twelve refused".
      if (outcome.rows.length > 0) {
        invite.showResults(outcome.rows);
        setToastType(outcome.summary.invited > 0 ? 'success' : 'error');
        setToastMessage(invitePartialMessage(outcome.summary));
        userListQuery.refetch?.();
        return;
      }
      invite.clearResults();
      setToastType('error');
      setToastMessage(errorMessage(error));
    },
    onEditSuccess: () => {
      setToastType('success');
      setToastMessage(t('shared.ui.settings.users.userEdited', 'The user has been edited successfully'));
    },
    onEditError: (error: unknown) => {
      setToastType('error');
      setToastMessage(errorMessage(error));
    },
  });

  const { handleInviteConfirm, singleAction, batchAction, actions } = actionsResult;

  // A new submit must not render the previous one's rows underneath it.
  const { clearResults } = invite;
  const handleInviteSubmit = useCallback(
    (data: { emails: string[]; roles: string[] }) => {
      clearResults();
      handleInviteConfirm(data);
    },
    [handleInviteConfirm, clearResults],
  );

  const isLoading = userListQuery.isFetching || roleListQuery.isFetching;
  const isError = userListQuery.isError || roleListQuery.isError;

  const handleRetry = useCallback(() => {
    userListQuery.refetch?.();
    roleListQuery.refetch?.();
  }, [userListQuery, roleListQuery]);

  // ── toast auto-clear ─────────────────────────────────────────────────
  useEffect(() => {
    if (!toastMessage) return;
    const timer = setTimeout(() => setToastMessage(''), 3000);
    return () => clearTimeout(timer);
  }, [toastMessage]);

  return (
    <UsersPageContent
      data={{
        users: pagedUsers,
        // Old-app parity (`Users.jsx`: `total={!search ? total : filteredUsers.length}`):
        // show the real server total while unfiltered, the loaded+matched count while searching.
        total: debouncedSearch ? filteredUsers.length : serverTotal,
        filteredUsers,
        selectedUsers,
      }}
      pagination={{ rowsPerPage: pageSize, page, pageSize }}
      tableActions={{
        onSearchChange: handleSearchChange,
        onPageSizeChange: handlePageSizeChange,
        onChangePage: handleChangePage,
        onSort: handleSort,
        onSelectPage: handleSelectPage,
        onSelectRow: handleSelectRow,
      }}
      sorting={{ sortField, sortDirection }}
      search={{ searchText }}
      toast={{ toastMessage, toastType }}
      dialogs={{
        inviteOpen: invite.open,
        actions,
        singleAction,
        batchAction,
        rolesOptions,
        onInviteConfirm: handleInviteSubmit,
        onSetInviteOpen: invite.setOpen,
        inviteResults: invite.results,
      }}
      permissions={{ canView, canCreate, canEdit, canDelete }}
      status={{ isError, permissionsResolved, onRetry: handleRetry }}
      isLoading={isLoading}
    />
  );
}
