/**
 * The project budget table for Admin → Budgets.
 *
 * ## Two limits, and they are not the same column
 *
 * `monthly_limit` is what an operator AUTHORED; `effective_limit` is what the
 * gateway ENFORCES. They differ whenever a project is `enabled: false`, which
 * keeps the number and admits every call. Showing one number would make an
 * exemption read as an active ceiling, so the Limit column renders the authored
 * figure and marks it inactive when nothing is enforcing it.
 *
 * ## Absent is not zero
 *
 * `spend_available: false` means no accumulator row exists for this period,
 * which is a fact about the write-back pipeline rather than about the project.
 * It renders as a dash. Rendering `$0.00` there would claim the project made no
 * billed calls, which is a different and possibly wrong statement.
 *
 * `percent_used` is null for an unlimited project — there is no denominator —
 * and also renders as a dash rather than 0%.
 */
import { memo, useMemo } from 'react';

import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlineOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import GroupOutlinedIcon from '@mui/icons-material/GroupOutlined';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { GridColDef, GridRenderCellParams } from '@mui/x-data-grid';
import { DataGrid } from '@mui/x-data-grid';

import { t } from '@/shared/i18n';

import type { ProjectBudgetRow } from './api/adminBudgetsApi';

export interface AdminBudgetsTableProps {
  readonly rows: readonly ProjectBudgetRow[];
  readonly isLoading: boolean;
  readonly onEdit: (row: ProjectBudgetRow) => void;
  readonly onClear: (row: ProjectBudgetRow) => void;
  readonly onOpenMembers: (row: ProjectBudgetRow) => void;
}

const DASH = '—';

/**
 * Money, or a dash when there is none to show.
 *
 * `null` and `undefined` are both "the server did not give a number here", and
 * both must stay visibly empty rather than becoming 0.00.
 */
function money(value: number | null | undefined): string {
  if (value === null || value === undefined) return DASH;
  return `$${value.toFixed(2)}`;
}

function percent(value: number | null | undefined): string {
  if (value === null || value === undefined) return DASH;
  return `${value.toFixed(1)}%`;
}

/** A row is enforcing only when the gateway holds a ceiling for it. */
function isEnforcing(row: ProjectBudgetRow): boolean {
  return row.limit_source === 'explicit' && row.effective_limit !== null;
}

/** A row has something to clear only when somebody authored a ceiling. */
function hasAuthoredBudget(row: ProjectBudgetRow): boolean {
  return row.monthly_limit !== null && row.monthly_limit !== undefined;
}

export const AdminBudgetsTable = memo(function AdminBudgetsTable({
  rows,
  isLoading,
  onEdit,
  onClear,
  onOpenMembers,
}: AdminBudgetsTableProps) {
  const columns: GridColDef<ProjectBudgetRow>[] = useMemo(
    () => [
      {
        field: 'display_name',
        headerName: t('pages.admin.budgets.column.project', 'Project'),
        flex: 1.4,
        minWidth: 200,
        sortable: false,
        renderCell: (params: GridRenderCellParams<ProjectBudgetRow>) => (
          <Box sx={{ display: 'flex', flexDirection: 'column', justifyContent: 'center', height: '100%' }}>
            <Typography variant="bodyMedium">{params.row.display_name}</Typography>
            {params.row.is_personal ? (
              <Typography variant="bodySmall" color="text.secondary">
                {t('pages.admin.budgets.personal', 'Personal project')}
              </Typography>
            ) : null}
          </Box>
        ),
      },
      {
        field: 'owner_email',
        headerName: t('pages.admin.budgets.column.owner', 'Owner'),
        flex: 1.2,
        minWidth: 180,
        sortable: false,
        renderCell: (params: GridRenderCellParams<ProjectBudgetRow>) => (
          <Typography variant="bodyMedium" color="text.secondary">
            {params.row.owner_email !== '' ? params.row.owner_email : params.row.owner_name}
          </Typography>
        ),
      },
      {
        field: 'monthly_limit',
        headerName: t('pages.admin.budgets.column.limit', 'Limit'),
        width: 150,
        sortable: false,
        renderCell: (params: GridRenderCellParams<ProjectBudgetRow>) => {
          const authored = money(params.row.monthly_limit);
          if (authored === DASH) {
            return (
              <Typography variant="bodyMedium" color="text.secondary">
                {t('pages.admin.budgets.unlimited', 'Unlimited')}
              </Typography>
            );
          }
          // An authored ceiling the gateway is NOT applying. Saying so beside
          // the number is the whole reason both columns exist.
          return (
            <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.375rem', height: '100%' }}>
              <Typography variant="bodyMedium">{authored}</Typography>
              {isEnforcing(params.row) ? null : (
                <Chip
                  size="small"
                  variant="outlined"
                  color="warning"
                  label={t('pages.admin.budgets.exempt', 'Not enforced')}
                />
              )}
            </Box>
          );
        },
      },
      {
        field: 'spend',
        headerName: t('pages.admin.budgets.column.spend', 'Spend this period'),
        width: 160,
        sortable: false,
        renderCell: (params: GridRenderCellParams<ProjectBudgetRow>) => (
          <Typography variant="bodyMedium">
            {params.row.spend_available === false ? DASH : money(params.row.spend)}
          </Typography>
        ),
      },
      {
        field: 'percent_used',
        headerName: t('pages.admin.budgets.column.used', 'Used'),
        width: 100,
        sortable: false,
        renderCell: (params: GridRenderCellParams<ProjectBudgetRow>) => (
          <Typography variant="bodyMedium">{percent(params.row.percent_used)}</Typography>
        ),
      },
      {
        field: 'actions',
        headerName: t('pages.admin.budgets.column.actions', 'Actions'),
        width: 150,
        sortable: false,
        renderCell: (params: GridRenderCellParams<ProjectBudgetRow>) => (
          <Box sx={{ display: 'flex', alignItems: 'center', height: '100%' }}>
            <Tooltip title={t('pages.admin.budgets.action.edit', 'Edit budget')}>
              <IconButton
                size="small"
                onClick={() => onEdit(params.row)}
                data-testid={`admin-budgets-edit-${params.row.project_id}`}
              >
                <EditOutlinedIcon fontSize="small" />
              </IconButton>
            </Tooltip>
            <Tooltip title={t('pages.admin.budgets.action.members', 'Member budgets')}>
              <IconButton
                size="small"
                onClick={() => onOpenMembers(params.row)}
                data-testid={`admin-budgets-members-${params.row.project_id}`}
              >
                <GroupOutlinedIcon fontSize="small" />
              </IconButton>
            </Tooltip>
            {/* Clearing is offered only where there is something to clear: a
                project with no authored row is already at the default. */}
            {hasAuthoredBudget(params.row) ? (
              <Tooltip title={t('pages.admin.budgets.action.clear', 'Clear back to default')}>
                <IconButton
                  size="small"
                  onClick={() => onClear(params.row)}
                  data-testid={`admin-budgets-clear-${params.row.project_id}`}
                >
                  <DeleteOutlineIcon fontSize="small" />
                </IconButton>
              </Tooltip>
            ) : null}
          </Box>
        ),
      },
    ],
    [onEdit, onClear, onOpenMembers],
  );

  return (
    <DataGrid
      rows={rows}
      columns={columns}
      loading={isLoading}
      rowHeight={56}
      hideFooter
      disableRowSelectionOnClick
      getRowId={(row: ProjectBudgetRow) => row.project_id}
      sx={{ border: 'none', flex: 1, minHeight: '12rem' }}
    />
  );
});
