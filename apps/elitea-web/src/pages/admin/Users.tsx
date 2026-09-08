/**
 * Admin › Users — the GLOBAL user administration page (unit A14, issue #200).
 *
 * Reference (read-only): `frontends/admin_ui/frontend/src/pages/UsersPage/`.
 * Rewritten against this app's stack: Redux Toolkit → TanStack Query
 * (`./api/adminUsersApi`), axios → `eliteaFetch`, react-router → TanStack
 * Router (`./router`), MUI 7 → 9, and this app's `shared/ui` primitives instead
 * of admin_ui's own component set. State and handlers live in
 * `./useAdminUsersPage`.
 *
 * ## What is real and what is not
 *
 * Three of the reference page's four row actions had NO server when this unit
 * started. All four are now live; each was resolved deliberately:
 *
 *  - **delete**, **set admin role**, **suspend/unsuspend** — implemented for
 *    real in `services/elitea-main/internal/api/v2/admin/users.go`, covered by
 *    write-then-re-read tests. Live controls.
 *  - **user activity** — live, as of the per-user activity port. A14's Audit
 *    Trail page gave elitea-main a real audit API whose four endpoints all take
 *    `user_id`; this page's row control opens `./UserActivityDrawer` over it.
 *    It shipped disabled only because the VIEW was missing, never the data.
 *  - **export** — real, and the one place this port deliberately differs in
 *    FORMAT from the reference: it writes CSV, not .xlsx, because this app
 *    carries no spreadsheet dependency (see `./adminUsersCsv`). The control
 *    says "Export to CSV" so the button never promises a file type it does
 *    not produce.
 *
 * Nothing here is a button that no-ops.
 *
 * ## Authorisation
 *
 * `window.admin_ui_config.permissions` is presentation state and never a gate —
 * see `./adminUiConfig`. Every mutation is authorised server-side on each
 * request; the flags here only decide what is worth rendering.
 */
import { useState } from 'react';

import DeleteIcon from '@mui/icons-material/Delete';
import FileDownloadOutlinedIcon from '@mui/icons-material/FileDownloadOutlined';
import GroupAddOutlinedIcon from '@mui/icons-material/GroupAddOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminBulkInviteDialog } from './AdminBulkInviteDialog';
import { AdminUsersTable } from './AdminUsersTable';
import { UserActivityDrawer } from './UserActivityDrawer';
import { ADMIN_USERS_PAGE_SIZE, useAdminUsersPage } from './useAdminUsersPage';


export function AdminUsers() {
  const state = useAdminUsersPage();
  // The cross-project bulk invite (issue 247). It lives on this page because
  // its subject is ACCOUNTS: pylon put "add every user to a project" under
  // Invites, beside the global user list, and the picker it needs is the very
  // list this page already holds.
  const [bulkInviteOpen, setBulkInviteOpen] = useState(false);

  const { total, page, deleteIds, rows } = state;
  const lastPage = total === 0 ? 0 : Math.ceil(total / ADMIN_USERS_PAGE_SIZE) - 1;
  const firstShown = total === 0 ? 0 : page * ADMIN_USERS_PAGE_SIZE + 1;
  const lastShown = Math.min((page + 1) * ADMIN_USERS_PAGE_SIZE, total);

  const deleteTargetName =
    deleteIds.length === 1
      ? (rows.find((row) => row.id === deleteIds[0])?.email ?? String(deleteIds[0]))
      : `${deleteIds.length} ${t('pages.admin.users.deleteModal.users', 'users')}`;

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }}>
      <Box
        sx={{
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'center',
          gap: '1rem',
          flexWrap: 'wrap',
        }}
      >
        <Typography variant="h5" sx={{ fontWeight: 600 }}>
          {t('pages.admin.users.title', 'Users')}
        </Typography>
        {/*
          The search box is the ONLY item allowed to shrink here. Before this,
          every child was shrinkable, so at a narrow viewport the browser took
          the width out of the Delete button instead — its label wrapped to two
          lines inside a control whose height is a fixed 1.75rem (MuiButton.root),
          and the text spilled out of the pill.
        */}
        <Box
          sx={{
            display: 'flex',
            alignItems: 'center',
            gap: '0.5rem',
            flex: '1 1 auto',
            justifyContent: 'flex-end',
            minWidth: 0,
          }}
        >
          <Box sx={{ flex: '1 1 12rem', minWidth: '8rem', maxWidth: '20rem' }}>
            <SimpleSearchBar
              value={state.search}
              onChange={state.onSearchChange}
              placeholder={t('pages.admin.users.search', 'Search by name or email')}
              data-testid="admin-users-search"
            />
          </Box>
          {state.onSelectionChange && state.selectedIds.length > 0 ? (
            <Button
              variant="elitea"
              color="alarm"
              size="small"
              startIcon={<DeleteIcon fontSize="small" />}
              onClick={() => state.onRequestDelete(state.selectedIds)}
              sx={{ flexShrink: 0, whiteSpace: 'nowrap' }}
            >
              {`${t('pages.admin.users.action.deleteSelected', 'Delete')} (${state.selectedIds.length})`}
            </Button>
          ) : null}
          <Button
            variant="elitea"
            color="tertiary"
            size="small"
            startIcon={<GroupAddOutlinedIcon fontSize="small" />}
            onClick={() => setBulkInviteOpen(true)}
            sx={{ flexShrink: 0, whiteSpace: 'nowrap' }}
            data-testid="admin-bulk-invite-open"
          >
            {t('pages.admin.users.action.bulkInvite', 'Bulk invite')}
          </Button>
          <Tooltip title={t('pages.admin.users.action.export', 'Export to CSV')}>
            {/* `span`: a disabled button fires no events, so the tooltip needs
                a wrapper to hang its listeners on while the export runs. */}
            <span>
              <IconButton
                onClick={state.onExport}
                disabled={state.isExporting}
                sx={{ flexShrink: 0 }}
                aria-label={t('pages.admin.users.action.export', 'Export to CSV')}
              >
                {state.isExporting ? (
                  <CircularProgress size={16} />
                ) : (
                  <FileDownloadOutlinedIcon fontSize="small" />
                )}
              </IconButton>
            </span>
          </Tooltip>
        </Box>
      </Box>

      <Tabs value={state.activeTab} onChange={state.onTabChange} sx={{ minHeight: '2.5rem' }}>
        <Tab
          label={`${t('pages.admin.users.tab.platform', 'Platform Users')} (${state.counts.platform})`}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
        <Tab
          label={`${t('pages.admin.users.tab.system', 'System Users')} (${state.counts.system})`}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
      </Tabs>

      {state.errorMessage ? (
        <Alert severity="error" onClose={state.onDismissError}>
          {state.errorMessage}
        </Alert>
      ) : null}

      {state.isError ? (
        <Alert severity="error">{t('pages.admin.users.error.load', 'Failed to load users.')}</Alert>
      ) : (
        <Box sx={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
          <AdminUsersTable
            users={state.rows}
            isLoading={state.isFetching}
            selectedIds={state.selectedIds}
            onSelectionChange={state.onSelectionChange}
            sortField={state.sortField}
            sortDirection={state.sortDirection}
            onSort={state.onSort}
            rowActions={state.rowActions}
            canAssignSuperAdmin={state.canAssignSuperAdmin}
            pendingIds={state.pendingIds}
          />

          <Box
            sx={{
              display: 'flex',
              justifyContent: 'flex-end',
              alignItems: 'center',
              gap: '0.75rem',
              paddingTop: '0.5rem',
            }}
          >
            <Typography variant="bodyMedium" color="text.secondary">
              {`${firstShown}–${lastShown} / ${total}`}
            </Typography>
            <Button
            variant="elitea" color="tertiary" size="small" disabled={page === 0} onClick={state.onPreviousPage}>
              {t('pages.admin.users.pagination.previous', 'Previous')}
            </Button>
            <Button
            variant="elitea" color="tertiary" size="small" disabled={page >= lastPage} onClick={state.onNextPage}>
              {t('pages.admin.users.pagination.next', 'Next')}
            </Button>
          </Box>
        </Box>
      )}

      <DeleteEntityModal
        open={deleteIds.length > 0}
        onClose={state.onCancelDelete}
        onConfirm={state.onConfirmDelete}
        confirming={state.isDeleting}
        name={deleteTargetName}
        copy={{ title: t('pages.admin.users.deleteModal.title', 'Delete confirmation') }}
      />

      <AdminBulkInviteDialog open={bulkInviteOpen} onClose={() => setBulkInviteOpen(false)} />

      <UserActivityDrawer user={state.activityUser} onClose={state.onCloseActivity} />
    </DrawerPage>
  );
}
