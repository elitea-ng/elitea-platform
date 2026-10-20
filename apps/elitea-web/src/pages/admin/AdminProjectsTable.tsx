/**
 * The project table for the admin Projects page (unit A14).
 *
 * Reference: `frontends/admin_ui/frontend/src/pages/ProjectsPage/ProjectsTable.jsx`
 * (read-only). This is a rewrite, not a copy — that component is MUI 7 over a
 * bespoke `GridTable` plus a `useResponsiveColumns` hook that reads
 * `window.innerWidth` once at render; here the columns are MUI X DataGrid
 * `flex` definitions, matching `./AdminUsersTable` and the project-level
 * `features/settings/ui/users/UsersTable.tsx`.
 *
 * ## Row selection feeds the one destructive control on the page
 *
 * The checkbox column exists to arm `Projects.tsx`'s bulk delete, which reaches
 * `DELETE /projects/project/administration/{id}` — the provisioning pipeline in
 * reverse, ending in `DROP SCHEMA p_<id> CASCADE`. Selection is therefore
 * rendered ONLY when the page passes `onSelectionChange`, which it does only
 * for an operator whose config advertises the delete permission. An operator
 * who cannot delete gets no checkboxes rather than checkboxes that arm nothing.
 *
 * There is deliberately no per-row bin. One destructive path, one confirmation
 * dialog, one place that lists what is about to be destroyed — a second entry
 * point would be a second chance to skip that list.
 */
import BlockOutlinedIcon from '@mui/icons-material/BlockOutlined';
import CheckCircleOutlinedIcon from '@mui/icons-material/CheckCircleOutlined';
import CloudOutlinedIcon from '@mui/icons-material/CloudOutlined';
import PersonAddAlt1OutlinedIcon from '@mui/icons-material/PersonAddAlt1Outlined';
import TimelineOutlinedIcon from '@mui/icons-material/TimelineOutlined';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import { useTheme } from '@mui/material/styles';
import type {
  GridColDef,
  GridRenderCellParams,
  GridRowSelectionModel,
  GridSortModel,
} from '@mui/x-data-grid';
import { DataGrid } from '@mui/x-data-grid';
import { memo, useMemo } from 'react';

import { t } from '@/shared/i18n';

import { PROJECT_STATUS_COLOUR, projectStatusLabel } from './adminProjectsStatus';
import type { AdminProjectRow } from './api/adminProjectsApi';

export interface AdminProjectsTableProps {
  projects: readonly AdminProjectRow[];
  isLoading: boolean;
  sortField: string;
  sortDirection: 'asc' | 'desc';
  onSort: (field: string, direction: 'asc' | 'desc') => void;
  /** Absent ⇒ the control is not rendered at all (this user may not write). */
  onToggleSuspended: ((project: AdminProjectRow) => void) | undefined;
  onOpenMembers: ((project: AdminProjectRow) => void) | undefined;
  /** Always present: the activity drawer is a read. */
  onOpenActivity: (project: AdminProjectRow) => void;
  /** The project's Send to cloud switch. Absent ⇒ no control (callers that only pick projects). */
  onOpenCloudExecutor?: ((project: AdminProjectRow) => void) | undefined;
  /** Ids whose mutation is in flight — their per-row controls are disabled. */
  pendingIds: ReadonlySet<number>;
  /** The currently selected ids. Meaningless without `onSelectionChange`. */
  selectedIds: readonly number[];
  /** Absent ⇒ no checkbox column at all (this user may not delete). */
  onSelectionChange: ((ids: number[]) => void) | undefined;
}

/** The admin list is joined for display; the tooltip carries the whole set. */
/**
 * The storage a project's cloud copies use, in binary units. `—` when the
 * server did not send the field.
 */
export function formatWorkspaceBytes(bytes: number | undefined): string {
  if (bytes === undefined || !Number.isFinite(bytes) || bytes < 0) return '—';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return unit === 0 ? `${value} B` : `${value.toFixed(value < 10 ? 1 : 0)} ${units[unit]}`;
}

function renderAdmins(names: readonly string[]): React.ReactElement {
  const text = names.length > 0 ? names.join(', ') : '—';
  return (
    <Tooltip title={names.length > 1 ? text : ''}>
      <Typography
        variant="bodyMedium"
        color="text.secondary"
        sx={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
      >
        {text}
      </Typography>
    </Tooltip>
  );
}

export const AdminProjectsTable = memo(function AdminProjectsTable({
  projects,
  isLoading,
  sortField,
  sortDirection,
  onSort,
  onToggleSuspended,
  onOpenMembers,
  onOpenActivity,
  onOpenCloudExecutor,
  pendingIds,
  selectedIds,
  onSelectionChange,
}: AdminProjectsTableProps) {
  const theme = useTheme();

  // MUI X 9's selection model is `{type, ids}`, not the flat id array v7 took.
  // Derived from the page's state rather than held here, so the toolbar's
  // "delete N" count and the ticked boxes cannot disagree.
  const selectionModel: GridRowSelectionModel = useMemo(
    () => ({ type: 'include', ids: new Set<string | number>(selectedIds) }),
    [selectedIds],
  );

  const sortModel: GridSortModel = useMemo(
    () => (sortField ? [{ field: sortField, sort: sortDirection }] : []),
    [sortField, sortDirection],
  );

  const columns: GridColDef<AdminProjectRow>[] = useMemo(
    () => [
      {
        field: 'name',
        headerName: t('pages.admin.projects.column.name', 'Name'),
        flex: 1.2,
        minWidth: 160,
        sortable: true,
      },
      {
        field: 'id',
        headerName: t('pages.admin.projects.column.id', 'ID'),
        width: 90,
        sortable: true,
      },
      {
        field: 'owner_name',
        headerName: t('pages.admin.projects.column.owner', 'Owner'),
        flex: 1,
        minWidth: 140,
        // Not on the server's sort allow-list (`sortableProjectColumns`), and an
        // unknown `sort_by` there falls back to `name` — a header that silently
        // sorted by something else is worse than one that does not sort.
        sortable: false,
        renderCell: (params: GridRenderCellParams<AdminProjectRow>) => (
          <Typography variant="bodyMedium" color="text.secondary">
            {params.row.owner_name || '—'}
          </Typography>
        ),
      },
      {
        field: 'admin_names',
        headerName: t('pages.admin.projects.column.admins', 'Admins'),
        flex: 1,
        minWidth: 140,
        sortable: false,
        renderCell: (params: GridRenderCellParams<AdminProjectRow>) =>
          renderAdmins(params.row.admin_names),
      },
      {
        field: 'workspace_bytes',
        headerName: t('pages.admin.projects.column.workspaceBytes', 'Cloud copies'),
        width: 130,
        // On the server's sort allow-list (`sortableProjectColumns`).
        sortable: true,
        renderCell: (params: GridRenderCellParams<AdminProjectRow>) => (
          <Typography variant="bodyMedium" color="text.secondary" data-testid="admin-project-workspace-bytes">
            {formatWorkspaceBytes(params.row.workspace_bytes)}
          </Typography>
        ),
      },
      {
        field: 'status',
        headerName: t('pages.admin.projects.column.status', 'Status'),
        width: 120,
        sortable: true,
        renderCell: (params: GridRenderCellParams<AdminProjectRow>) => (
          <Chip
            size="small"
            variant="outlined"
            color={PROJECT_STATUS_COLOUR[params.row.status]}
            label={projectStatusLabel(params.row.status)}
          />
        ),
      },
      {
        field: 'actions',
        headerName: t('pages.admin.projects.column.actions', 'Actions'),
        width: 172,
        sortable: false,
        disableColumnMenu: true,
        renderCell: (params: GridRenderCellParams<AdminProjectRow>) => {
          const row = params.row;
          const busy = pendingIds.has(row.id);
          const suspendLabel = row.suspended
            ? t('pages.admin.projects.action.unsuspend', 'Unsuspend project')
            : t('pages.admin.projects.action.suspend', 'Suspend project');
          const activityLabel = t('pages.admin.projects.action.activity', 'Project activity');
          const membersLabel = t('pages.admin.projects.action.members', 'Manage project member');
          const cloudLabel = t('features.cloudExecutorSwitch.admin.action', 'Send to cloud (cloud executor)');
          return (
            <Box sx={{ display: 'flex', gap: 0.25 }} onClick={(event) => event.stopPropagation()}>
              {onOpenMembers ? (
                <Tooltip title={membersLabel}>
                  <span>
                    <IconButton size="small" aria-label={membersLabel} onClick={() => onOpenMembers(row)}>
                      <PersonAddAlt1OutlinedIcon fontSize="small" />
                    </IconButton>
                  </span>
                </Tooltip>
              ) : null}

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

              <Tooltip title={activityLabel}>
                <span>
                  <IconButton size="small" aria-label={activityLabel} onClick={() => onOpenActivity(row)}>
                    <TimelineOutlinedIcon fontSize="small" />
                  </IconButton>
                </span>
              </Tooltip>

              {onOpenCloudExecutor ? (
                <Tooltip title={cloudLabel}>
                  <span>
                    <IconButton size="small" aria-label={cloudLabel} onClick={() => onOpenCloudExecutor(row)}>
                      <CloudOutlinedIcon fontSize="small" />
                    </IconButton>
                  </span>
                </Tooltip>
              ) : null}
            </Box>
          );
        },
      },
    ],
    [onToggleSuspended, onOpenMembers, onOpenActivity, onOpenCloudExecutor, pendingIds],
  );

  if (!isLoading && projects.length === 0) {
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
        <Typography variant="bodyMedium">
          {t('pages.admin.projects.empty', 'No projects')}
        </Typography>
      </Box>
    );
  }

  return (
    <DataGrid
      rows={projects}
      columns={columns}
      loading={isLoading}
      rowHeight={48}
      hideFooter
      getRowId={(row: AdminProjectRow) => row.id}
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
      getRowClassName={(params) => (params.row.suspended ? 'admin-projects-row--suspended' : '')}
      sx={{
        flex: 1,
        minHeight: 0,
        border: 'none',
        '& .admin-projects-row--suspended': { opacity: 0.55 },
      }}
    />
  );
});
