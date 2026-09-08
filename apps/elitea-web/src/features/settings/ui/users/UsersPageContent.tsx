// @ts-nocheck
/**
 * UsersPageContent — renders the users page UI (search, table, pagination,
 * dialogs).
 *
 * Extracted from the `Users` settings page to keep that file under 400 lines.
 *
 * Prop budget (≤ 12 §3.5) maintained via grouped interfaces.
 */
import { memo } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';

import type { UserRecord } from '@/shared/api/generated/model';
import type { EditUsersButtonProps } from '@/shared/ui/settings/EditUsersButton';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { InviteUserDialog } from '@/shared/ui/settings/InviteUserDialog';
import type { InviteAddressResult } from '@/shared/ui/settings/inviteResults';
import { UsersTable } from './UsersTable';
import { UsersPageHeader } from './UsersPageHeader';
import { UsersPagePagination } from './UsersPagePagination';
import { t } from '@/shared/i18n';
import { usersPageStyles } from './UsersPage.styles';

// ---------------------------------------------------------------------------
// Grouped prop interfaces (§3.5 component-props budget)
// ---------------------------------------------------------------------------

interface PageData {
  users: UserRecord[];
  total: number;
  filteredUsers: UserRecord[];
  selectedUsers: UserRecord[];
}

interface PaginationState {
  rowsPerPage: number;
  page: number;
  pageSize: number;
}

interface TableActions {
  onSearchChange: (e: React.ChangeEvent<HTMLInputElement>) => void;
  onPageSizeChange: (size: number) => void;
  onChangePage: (page: number) => void;
  onSort: (field: string, direction: 'asc' | 'desc') => void;
  onSelectPage: (selected: boolean) => void;
  onSelectRow: (user: { id: string }, selected: boolean) => void;
}

interface SortingState {
  sortField: string;
  sortDirection: 'asc' | 'desc';
}

interface SearchState {
  searchText: string;
}

interface ToastState {
  toastMessage: string;
  toastType: 'success' | 'error';
}

/** RBAC gates ported from the old app's `checkPermission(PERMISSIONS.users.*)` calls (spec §9.3). */
interface PermissionState {
  canView: boolean;
  canCreate: boolean;
  canEdit: boolean;
  canDelete: boolean;
}

/**
 * Whether the member list can be shown at all, and why not.
 *
 * `permissionsResolved` is separate from `permissions.canView` on purpose:
 * `usePermissionSet` answers an EMPTY set while the permission request is
 * still in flight, so `canView === false` means "denied OR not known yet".
 * Without this flag every user sees the no-permission banner flash before
 * the table arrives.
 */
interface StatusState {
  isError: boolean;
  permissionsResolved: boolean;
  onRetry: () => void;
}

interface DialogActions {
  inviteOpen: boolean;
  actions: { edit: EditUsersButtonProps | null; delete: Record<string, unknown> };
  singleAction: { edit?: { userIds?: Set<string>; userRoles?: Set<string>; onConfirm: (roles: Set<string>) => void; isLoading?: boolean; disabled?: boolean; rolesOptions?: Array<{ label: string; value: string }> } };
  batchAction: { edit?: { userIds?: Set<string>; userRoles?: Set<string>; onConfirm: (roles: Set<string>) => void; isLoading?: boolean; disabled?: boolean; rolesOptions?: Array<{ label: string; value: string }> } };
  rolesOptions: Array<{ label: string; value: string }>;
  onInviteConfirm: (roles: Set<string>) => void;
  onSetInviteOpen: (open: boolean) => void;
  /** The server's per-address answer to the last submit; empty before the first. */
  inviteResults?: readonly InviteAddressResult[];
}

export interface UsersPageContentProps {
  data: PageData;
  pagination: PaginationState;
  tableActions: TableActions;
  sorting: SortingState;
  search: SearchState;
  toast: ToastState;
  dialogs: DialogActions;
  permissions: PermissionState;
  status: StatusState;
  isLoading?: boolean;
}

export const UsersPageContent = memo(function UsersPageContent({
  data, pagination, tableActions, sorting, search, toast, dialogs, permissions, status, isLoading,
}: UsersPageContentProps) {
  const startRow = pagination.page * pagination.rowsPerPage + 1;
  // ONE prop, not two. `startRow` and `endRow` are a single range, and the
  // §3.5 component-props budget is 12. Passing them apart put UsersPageBody at
  // 13 and failed `scripts/check-budgets.mjs`.
  const rowRange: RowRange = {
    start: startRow,
    end: Math.min(startRow + pagination.rowsPerPage - 1, data.total),
  };

  return (
    <UsersPageBody
      styles={usersPageStyles}
      data={data}
      pagination={pagination}
      tableActions={tableActions}
      sorting={sorting}
      search={search}
      toast={toast}
      dialogs={dialogs}
      permissions={permissions}
      status={status}
      isLoading={isLoading}
      rowRange={rowRange}
    />
  );
});

/* ── sub-component: render body ───────────────────────────────────────── */

/** The 1-based row window the pagination line reports. */
interface RowRange {
  start: number;
  end: number;
}

interface UsersPageBodyProps {
  styles: typeof import('./UsersPage.styles').usersPageStyles;
  data: PageData;
  pagination: PaginationState;
  tableActions: TableActions;
  sorting: SortingState;
  search: SearchState;
  toast: ToastState;
  dialogs: DialogActions;
  permissions: PermissionState;
  status: StatusState;
  isLoading?: boolean;
  rowRange: RowRange;
}

function UsersPageBody({
  styles, data, pagination, tableActions, sorting, search, toast, dialogs, permissions, status,
  isLoading, rowRange,
}: UsersPageBodyProps) {
  return (
    <Box sx={styles.container}>
      <UsersPageHeader
        usersPageStyles={styles}
        searchText={search.searchText}
        onSearchChange={tableActions.onSearchChange}
        actions={dialogs.actions}
        selectedUsers={data.selectedUsers}
        onSetInviteOpen={dialogs.onSetInviteOpen}
        permissions={permissions}
      />
      <UsersPageToast message={toast.toastMessage} toastType={toast.toastType} />
      <UsersPageMain
        styles={styles}
        data={data}
        pagination={pagination}
        tableActions={tableActions}
        sorting={sorting}
        dialogs={dialogs}
        permissions={permissions}
        status={status}
        isLoading={isLoading}
        rowRange={rowRange}
      />
      <UsersPageDialogs
        rolesOptions={dialogs.rolesOptions}
        onSetInviteOpen={dialogs.onSetInviteOpen}
        onInviteConfirm={dialogs.onInviteConfirm}
        inviteOpen={dialogs.inviteOpen}
        inviteResults={dialogs.inviteResults}
      />
    </Box>
  );
}

/**
 * The member list, or the reason there is no member list.
 *
 * The table block used to be wrapped in a bare `{permissions.canView && (…)}`
 * with no `else`, and the page never read `isError`. Both a 403 and a failed
 * request therefore rendered as blank space under the header: the user could
 * not tell whether the project has no members, whether they lack access, or
 * whether the request failed. The error branch REPLACES the table on purpose.
 * `UsersTable` renders its "No users" placeholder for an empty list. That
 * placeholder states a fact that is not known to be true when the fetch
 * failed.
 */
function UsersPageMain({
  styles, data, pagination, tableActions, sorting, dialogs, permissions, status,
  isLoading, rowRange,
}: Omit<UsersPageBodyProps, 'search' | 'toast'>) {
  if (!status.permissionsResolved) return null;

  if (!permissions.canView) {
    return (
      <BannerMessage
        variant="info"
        message={t('shared.ui.settings.users.noAccess', 'You do not have permission to view the project members.')}
      />
    );
  }

  if (status.isError) {
    return (
      <Alert
        severity="error"
        action={(
          <Button
            color="inherit"
            size="small"
            onClick={status.onRetry}
          >
            {t('shared.ui.settings.users.retry', 'Retry')}
          </Button>
        )}
      >
        {t('shared.ui.settings.users.loadFailed', 'The system did not load the project members.')}
      </Alert>
    );
  }

  return (
    <>
      <UsersPageTable
        usersPageStyles={styles}
        users={data.users}
        pagination={{
          total: data.total,
          rowsPerPage: pagination.rowsPerPage,
          page: pagination.page,
          onSelectPage: tableActions.onSelectPage,
        }}
        selectedUsers={data.selectedUsers}
        onSelectRow={tableActions.onSelectRow}
        sort={{ field: sorting.sortField, direction: sorting.sortDirection, onSort: tableActions.onSort }}
        actions={dialogs.actions}
        canEdit={permissions.canEdit}
        isLoading={isLoading}
      />
      {data.total > 0 && (
        <UsersPagePagination
          usersPageStyles={styles}
          startRow={rowRange.start}
          endRow={rowRange.end}
          pageSize={pagination.pageSize}
          page={pagination.page}
          total={data.total}
          onPageSizeChange={tableActions.onPageSizeChange}
          onChangePage={tableActions.onChangePage}
        />
      )}
    </>
  );
}

/* ── sub-components ────────────────────────────────────────────────────── */

function UsersPageToast({ message, toastType }: { message: string; toastType: 'success' | 'error' }) {
  if (!message) return null;
  return (
    <Box
      sx={{
        padding: '0.5rem 1rem',
        backgroundColor: toastType === 'success' ? 'success.light' : 'error.light',
        color: toastType === 'success' ? 'success.dark' : 'error.dark',
        fontSize: ({ typography }) => typography.headingSmall.fontSize,
        borderRadius: 'var(--el-shape-radiusSm, 4px)',
        alignSelf: 'center',
      }}
      role="alert"
    >
      {message}
    </Box>
  );
}

function UsersPageTable({
  usersPageStyles, users, pagination, selectedUsers,
  onSelectRow, sort,
  actions,
  canEdit,
  isLoading,
}: {
  usersPageStyles: typeof import('./UsersPage.styles').usersPageStyles;
  users: UserRecord[];
  pagination: { total: number; rowsPerPage: number; page: number; onSelectPage: (selected: boolean) => void };
  selectedUsers: UserRecord[];
  onSelectRow: (user: { id: string }, selected: boolean) => void;
  sort: { field: string; direction: 'asc' | 'desc'; onSort: (field: string, direction: 'asc' | 'desc') => void };
  actions: { edit: EditUsersButtonProps | null; delete: Record<string, unknown> };
  canEdit: boolean;
  isLoading?: boolean;
}) {
  return (
    <Box sx={usersPageStyles.tableContainer}>
      <UsersTable
        users={users}
        pagination={{
          total: pagination.total,
          rowsPerPage: pagination.rowsPerPage,
          page: pagination.page,
          onSelectPage: pagination.onSelectPage,
        }}
        selectedUsers={selectedUsers}
        onSelectRow={onSelectRow}
        sorting={{ onSort: sort.onSort, sortField: sort.field, sortDirection: sort.direction }}
        actions={actions}
        canEdit={canEdit}
        isLoading={isLoading}
      />
    </Box>
  );
}

function UsersPageDialogs({
  rolesOptions, onSetInviteOpen, onInviteConfirm, inviteOpen, inviteResults,
}: {
  rolesOptions: Array<{ label: string; value: string }>;
  onSetInviteOpen: (open: boolean) => void;
  onInviteConfirm: (roles: Set<string>) => void;
  inviteOpen: boolean;
  inviteResults?: readonly InviteAddressResult[];
}) {
  // NOTE(#130): a third `EditUserRolesDialog` used to live here, mounted with
  // `open={Boolean(singleAction?.edit || batchAction?.edit)}` and
  // `onClose={() => {}}`. Both halves were wrong: `singleAction` is non-null
  // for ANY single selected row, so ticking a member's checkbox threw an
  // "Edit roles" modal over the whole page unasked — and the no-op `onClose`
  // meant neither Cancel, Escape nor the backdrop could dismiss it. Because the
  // modal marks the rest of the app `aria-hidden`, the page's own Edit and
  // Delete controls also stopped existing as far as assistive tech (and
  // Playwright's role queries) were concerned, which is how it was found.
  //
  // It was also redundant: `EditUsersButton` — rendered per row AND in the
  // header for the batch case — already owns a properly-gated dialog of its
  // own, with the same `onConfirm` wiring. Deleting the stray one loses no
  // reachable behaviour, and `singleAction`/`batchAction`/`onConfirm` are no
  // longer taken as props here at all — the only dialog left is the invite one,
  // driven by `inviteOpen`.
  return (
    <>
      <InviteUserDialog
        open={inviteOpen}
        onClose={() => onSetInviteOpen(false)}
        rolesOptions={rolesOptions}
        onConfirm={onInviteConfirm}
        results={inviteResults}
      />
    </>
  );
}
