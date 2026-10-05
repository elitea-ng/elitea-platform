/**
 * The global user table for the admin Users page (unit A14).
 *
 * Reference: `frontends/admin_ui/frontend/src/pages/UsersPage/UsersTable.jsx`
 * (read-only). This is a rewrite, not a copy — that component is MUI 7 over a
 * bespoke `GridTable` plus a `useResponsiveColumns` hook that reads
 * `window.innerWidth` once at render; here the columns are MUI X DataGrid
 * `flex` definitions, matching the DataGrid this app's project-level
 * `features/settings/ui/users/UsersTable.tsx` already uses.
 *
 * Two behavioural corrections to the reference, both deliberate:
 *
 *  1. Status/suspend read `suspended` (boolean), not `status === 'suspended'`.
 *     `auth_core__user` has no `status` column and no response has ever carried
 *     one, so the reference's chip was permanently "Active" and its toggle
 *     permanently computed "suspend".
 *  2. The role `<select>` disables only what the SERVER will refuse
 *     (`super_admin` without the super-admin permission). It is a hint; the
 *     server re-checks the same rule and answers 403 regardless.
 */
import { memo, useMemo } from 'react';

import BlockOutlinedIcon from '@mui/icons-material/BlockOutlined';
import CheckCircleOutlinedIcon from '@mui/icons-material/CheckCircleOutlined';
import DeleteIcon from '@mui/icons-material/Delete';
import DevicesOutlinedIcon from '@mui/icons-material/DevicesOutlined';
import TimelineOutlinedIcon from '@mui/icons-material/TimelineOutlined';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import { useTheme } from '@mui/material/styles';
import type { GridColDef, GridRenderCellParams, GridRowSelectionModel, GridSortModel } from '@mui/x-data-grid';
import { DataGrid } from '@mui/x-data-grid';

import { t } from '@/shared/i18n';

import type { AdminRole, AdminUserRow } from './api/adminUsersApi';

/** `''` is the DOM value for "no administration role"; the wire value is `null`. */
const NO_ROLE = '' as const;

/** What the role `<Select>` holds: an administration role, or the empty "None" value. */
type RoleSelectValue = AdminRole | typeof NO_ROLE;

/**
 * The per-row controls. `undefined` ⇒ that control is not rendered at all —
 * "this tab has no delete" and "this operator may not delete" collapse into one
 * representation and so cannot disagree with each other.
 */
export interface AdminUserRowActions {
  readonly onSetAdminRole: ((userId: number, roleName: AdminRole | null) => void) | undefined;
  readonly onToggleSuspended: ((user: AdminUserRow) => void) | undefined;
  readonly onDelete: ((userIds: number[]) => void) | undefined;
  /** Always present: activity is a READ, so it exists on every tab. */
  readonly onOpenActivity: (user: AdminUserRow) => void;
  /**
   * Opens the native device drawer (ADR-0025 WP3). Optional: the page offers it
   * only to an operator holding the permission the device routes require.
   */
  readonly onOpenDevices?: ((user: AdminUserRow) => void) | undefined;
}

export interface AdminUsersTableProps {
  users: readonly AdminUserRow[];
  isLoading: boolean;
  selectedIds: readonly number[];
  onSelectionChange: ((ids: number[]) => void) | undefined;
  sortField: string;
  sortDirection: 'asc' | 'desc';
  onSort: (field: string, direction: 'asc' | 'desc') => void;
  /**
   * The per-row controls, as ONE object rather than four sibling props.
   *
   * Grouped because the §3.5 component-props budget is 12 and the flat form
   * reached 13 when the activity control became live. They are the coherent
   * set to group: every one of them acts on a single row, and the page builds
   * them together. It must be a MEMOISED object — this component is `memo`'d
   * and `columns` is a `useMemo` over it, so a fresh literal per render would
   * defeat both.
   */
  rowActions: AdminUserRowActions;
  /**
   * Presentation-only: `false` disables the `super_admin` option. The server
   * enforces the same rule and refuses regardless of what this says.
   */
  canAssignSuperAdmin: boolean;
  /** Ids whose mutation is in flight — their per-row controls are disabled. */
  pendingIds: ReadonlySet<number>;
}

function formatLastLogin(value: string | null): string {
  if (!value) return t('pages.admin.users.neverLoggedIn', 'Never');
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

export const AdminUsersTable = memo(function AdminUsersTable({
  users,
  isLoading,
  selectedIds,
  onSelectionChange,
  sortField,
  sortDirection,
  onSort,
  rowActions,
  canAssignSuperAdmin,
  pendingIds,
}: AdminUsersTableProps) {
  const theme = useTheme();

  const roleOptions = useMemo(
    () => [
      { value: NO_ROLE, label: t('pages.admin.users.role.none', 'None') },
      { value: 'viewer', label: t('pages.admin.users.role.viewer', 'Viewer') },
      { value: 'editor', label: t('pages.admin.users.role.editor', 'Editor') },
      { value: 'admin', label: t('pages.admin.users.role.admin', 'Admin') },
      { value: 'super_admin', label: t('pages.admin.users.role.superAdmin', 'Super Admin') },
    ],
    [],
  );

  const sortModel: GridSortModel = useMemo(
    () => (sortField ? [{ field: sortField, sort: sortDirection }] : []),
    [sortField, sortDirection],
  );

  const selectionModel: GridRowSelectionModel = useMemo(
    () => ({ type: 'include', ids: new Set<string | number>(selectedIds) }),
    [selectedIds],
  );

  const columns: GridColDef<AdminUserRow>[] = useMemo(() => {
    const definitions: GridColDef<AdminUserRow>[] = [
      {
        field: 'name',
        headerName: t('pages.admin.users.column.name', 'Name'),
        flex: 1,
        minWidth: 140,
        sortable: true,
      },
      {
        field: 'email',
        headerName: t('pages.admin.users.column.email', 'Email'),
        flex: 1.4,
        minWidth: 200,
        sortable: true,
      },
      {
        field: 'last_login',
        headerName: t('pages.admin.users.column.lastLogin', 'Last login'),
        flex: 1,
        minWidth: 160,
        sortable: true,
        renderCell: (params: GridRenderCellParams<AdminUserRow>) => (
          <Typography variant="bodyMedium" color="text.secondary">
            {formatLastLogin(params.row.last_login)}
          </Typography>
        ),
      },
      {
        field: 'suspended',
        headerName: t('pages.admin.users.column.status', 'Status'),
        width: 120,
        sortable: false,
        renderCell: (params: GridRenderCellParams<AdminUserRow>) =>
          params.row.suspended ? (
            <Chip
              size="small"
              variant="outlined"
              color="warning"
              label={t('pages.admin.users.status.suspended', 'Suspended')}
            />
          ) : (
            <Chip
              size="small"
              variant="outlined"
              color="success"
              label={t('pages.admin.users.status.active', 'Active')}
            />
          ),
      },
    ];

    const { onSetAdminRole, onToggleSuspended, onDelete, onOpenActivity, onOpenDevices } = rowActions;

    if (onSetAdminRole) {
      definitions.push({
        field: 'admin_role',
        headerName: t('pages.admin.users.column.adminRole', 'Admin Role'),
        width: 170,
        sortable: false,
        renderCell: (params: GridRenderCellParams<AdminUserRow>) => {
          const row = params.row;
          const rowIsSuperAdmin = row.admin_role === 'super_admin';
          // Changing a super-admin's role at all requires the permission — the
          // server refuses the REVOKE just as it refuses the grant.
          const locked = rowIsSuperAdmin && !canAssignSuperAdmin;
          return (
            <Box onClick={(event) => event.stopPropagation()}>
              <Select<RoleSelectValue>
                size="small"
                displayEmpty
                value={row.admin_role ?? NO_ROLE}
                disabled={locked || pendingIds.has(row.id)}
                inputProps={{ 'aria-label': t('pages.admin.users.column.adminRole', 'Admin Role') }}
                onChange={(event) => {
                  const next = event.target.value;
                  onSetAdminRole(row.id, next === NO_ROLE ? null : next);
                }}
                sx={{ minWidth: 140 }}
              >
                {roleOptions.map((option) => (
                  <MenuItem
                    key={option.value}
                    value={option.value}
                    disabled={option.value === 'super_admin' && !canAssignSuperAdmin}
                  >
                    {option.label}
                  </MenuItem>
                ))}
              </Select>
            </Box>
          );
        },
      });
    }

    // Unconditional: the suspend and delete controls are permission- and
    // tab-dependent, but the activity control is not, so this column is never
    // empty and is never skipped. It WAS skipped when it held only writes.
    definitions.push({
      field: 'actions',
      headerName: t('pages.admin.users.column.actions', 'Actions'),
      width: 150,
      sortable: false,
      disableColumnMenu: true,
      renderCell: (params: GridRenderCellParams<AdminUserRow>) => {
        const row = params.row;
        const busy = pendingIds.has(row.id);
        const suspendLabel = row.suspended
          ? t('pages.admin.users.action.unsuspend', 'Unsuspend user')
          : t('pages.admin.users.action.suspend', 'Suspend user');
        return (
          // The click must not reach the DataGrid row: with
          // `checkboxSelection` on, MUI X treats a row click as a selection
          // toggle, which deselects the very row an action belongs to (#130).
          <Box sx={{ display: 'flex', gap: 0.25 }} onClick={(event) => event.stopPropagation()}>
            {onToggleSuspended ? (
              <Tooltip title={suspendLabel}>
                <span>
                  <IconButton
                    size="small"
                    aria-label={suspendLabel}
                    disabled={busy}
                    onClick={() => onToggleSuspended(row)}
                  >
                    {row.suspended ? (
                      <CheckCircleOutlinedIcon fontSize="small" color="success" />
                    ) : (
                      <BlockOutlinedIcon fontSize="small" />
                    )}
                  </IconButton>
                </span>
              </Tooltip>
            ) : null}

            {/*
              User activity. Opens `UserActivityDrawer` — the same audit
              endpoints the Audit Trail page reads, with `user_id` pinned to
              this row. It shipped DISABLED while elitea-main served no audit
              API at all; it does now, so this is a live control.

              Rendered on BOTH tabs and regardless of the write permissions:
              it is a read, and the server authorises it on its own.
            */}
            <Tooltip title={t('pages.admin.users.action.activity', 'User activity')}>
              <span>
                <IconButton
                  size="small"
                  aria-label={t('pages.admin.users.action.activity', 'User activity')}
                  onClick={() => onOpenActivity(row)}
                >
                  <TimelineOutlinedIcon fontSize="small" />
                </IconButton>
              </span>
            </Tooltip>

            {onOpenDevices ? (
              <Tooltip title={t('pages.admin.users.action.devices', 'Devices')}>
                <span>
                  <IconButton
                    size="small"
                    aria-label={t('pages.admin.users.action.devices', 'Devices')}
                    onClick={() => onOpenDevices(row)}
                    data-testid={`admin-user-devices-${row.id}`}
                  >
                    <DevicesOutlinedIcon fontSize="small" />
                  </IconButton>
                </span>
              </Tooltip>
            ) : null}

            {onDelete ? (
              <Tooltip title={t('pages.admin.users.action.delete', 'Delete user')}>
                <span>
                  <IconButton
                    size="small"
                    aria-label={t('pages.admin.users.action.delete', 'Delete user')}
                    disabled={busy}
                    onClick={() => onDelete([row.id])}
                  >
                    <DeleteIcon fontSize="small" />
                  </IconButton>
                </span>
              </Tooltip>
            ) : null}
          </Box>
        );
      },
    });

    return definitions;
  }, [rowActions, canAssignSuperAdmin, pendingIds, roleOptions]);

  if (!isLoading && users.length === 0) {
    return (
      <Box
        sx={{
          display: 'flex',
          flex: 1,
          alignItems: 'center',
          justifyContent: 'center',
          padding: '3rem',
          color: theme.vars.palette.text.secondary,
        }}
      >
        <Typography variant="bodyMedium">{t('pages.admin.users.empty', 'No users')}</Typography>
      </Box>
    );
  }

  return (
    <DataGrid
      rows={users}
      columns={columns}
      loading={isLoading}
      rowHeight={48}
      hideFooter
      getRowId={(row: AdminUserRow) => row.id}
      checkboxSelection={onSelectionChange !== undefined}
      rowSelectionModel={selectionModel}
      onRowSelectionModelChange={(model: GridRowSelectionModel) => {
        onSelectionChange?.(Array.from(model.ids).map((id) => Number(id)));
      }}
      sortingMode="server"
      sortModel={sortModel}
      onSortModelChange={(model: GridSortModel) => {
        const next = model[0];
        if (!next?.field) return;
        onSort(next.field, next.sort === 'desc' ? 'desc' : 'asc');
      }}
      getRowClassName={(params) => (params.row.suspended ? 'admin-users-row--suspended' : '')}
      sx={{
        flex: 1,
        minHeight: 0,
        border: 'none',
        '& .admin-users-row--suspended': { opacity: 0.55 },
      }}
    />
  );
});
