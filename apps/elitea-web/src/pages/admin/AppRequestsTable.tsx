/**
 * The queue table for the admin App Requests page (unit A14).
 *
 * Reference: `frontends/admin_ui/frontend/src/pages/AppRequestsPage/AppRequestsPage.jsx`
 * (read-only). A rewrite, not a copy — that component is MUI 7 over a bespoke
 * `GridTable` plus a `useResponsiveColumns` hook that reads `window.innerWidth`
 * once at render; here the columns are MUI X DataGrid `flex` definitions,
 * matching `./AdminProjectsTable` and `./AdminUsersTable`.
 *
 * ## Two columns the reference gets wrong, corrected here
 *
 *  - "Application" renders `entity_id`, underscores replaced by spaces and
 *    capitalised. `entity_id` is an opaque catalogue key with no requirement to
 *    be legible: the shipped clients send `wikis_Wikis` (which renders as "Wikis
 *    Wikis") and, from elitea-web, a synthetic numeric hash. `issue_type` is the
 *    label the requesting client actually displayed for the entry, so that is
 *    what this column shows, with the key beneath it — the operator needs both,
 *    and neither is guessed at.
 *  - The reference has no column for `rejection_comment` at all, so a rejection
 *    reason is write-only: the moderator types it, the requester is told, and
 *    the queue itself can never show it again. It is rendered here on the
 *    rejected rows.
 *
 * ## The model-connection request `entity_id` convention
 *
 * `entity_id` is genuinely opaque for most issue types (`wikis_Wikis`, a
 * synthetic hash), so the subtitle above renders it verbatim. Model
 * connection requests encode it as `provider:<type>` or `model:<name>`
 * instead — `describeEntityId` below recognises those two prefixes and
 * renders a readable label ("Provider: openai", "Model: gpt-4o"); anything
 * that does not match either prefix falls through unchanged, which is every
 * `entity_id` this deployment has ever produced before this convention.
 */
import CancelOutlinedIcon from '@mui/icons-material/CancelOutlined';
import CheckCircleOutlineIcon from '@mui/icons-material/CheckCircleOutlineOutlined';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import { useTheme } from '@mui/material/styles';
import type { GridColDef, GridRenderCellParams, GridSortModel } from '@mui/x-data-grid';
import { DataGrid } from '@mui/x-data-grid';
import { memo, useMemo } from 'react';

import { t } from '@/shared/i18n';

import type { AppRequestRow, AppRequestStatus } from './api/adminAppRequestsApi';

export interface AppRequestsTableProps {
  requests: readonly AppRequestRow[];
  isLoading: boolean;
  sortField: string;
  sortDirection: 'asc' | 'desc';
  onSort: (field: string, direction: 'asc' | 'desc') => void;
  /** Absent ⇒ the control is not rendered at all (this user may not decide). */
  onApprove: ((request: AppRequestRow) => void) | undefined;
  onOpenReject: ((request: AppRequestRow) => void) | undefined;
  /** Ids whose decision is in flight. */
  pendingIds: ReadonlySet<number>;
}

const STATUS_COLOUR: Record<AppRequestStatus, 'warning' | 'success' | 'error'> = {
  pending: 'warning',
  approved: 'success',
  rejected: 'error',
};

function statusLabel(status: AppRequestStatus): string {
  switch (status) {
    case 'approved':
      return t('pages.admin.appRequests.status.approved', 'Approved');
    case 'rejected':
      return t('pages.admin.appRequests.status.rejected', 'Rejected');
    case 'pending':
      return t('pages.admin.appRequests.status.pending', 'Pending');
  }
}

/**
 * Timestamps arrive as the server's naive UTC strings (the column is `TIMESTAMP`
 * WITHOUT time zone, matching the legacy model). `Z` is appended so the browser
 * reads them as UTC rather than as local time — the same correction the Audit
 * Trail port makes, and without it every row is off by the viewer's offset.
 */
function formatTimestamp(value: string): string {
  if (!value) return '—';
  const parsed = new Date(/[Z+]|-\d\d:\d\d$/.test(value) ? value : `${value}Z`);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

/**
 * Renders `provider:<type>` / `model:<name>` as a readable label; anything
 * else (every non-model-request `entity_id` today) passes through unchanged.
 * See this module's header for the convention.
 *
 * The name half is `encodeURIComponent`-ed at the point this address is built
 * (`features/settings/ui/ai-configuration/RequestModelConnection.tsx`'s
 * `buildModelConnectionEntityId` — `entity_id` travels as a raw path segment,
 * so an unescaped `/` in a vendor-prefixed model id would split it), so it is
 * decoded back here for display. `decodeURIComponent` throws on a malformed
 * escape; falling back to the raw value on that keeps a corrupt one visible
 * instead of blanking the row.
 */
function describeEntityId(entityId: string): string {
  const separator = entityId.indexOf(':');
  if (separator <= 0) return entityId;
  const prefix = entityId.slice(0, separator);
  const rawValue = entityId.slice(separator + 1);
  if (!rawValue) return entityId;
  let value = rawValue;
  try {
    value = decodeURIComponent(rawValue);
  } catch {
    // Malformed percent-encoding — show the raw value rather than throw.
  }
  if (prefix === 'provider') {
    return `${t('pages.admin.appRequests.entity.provider', 'Provider')}: ${value}`;
  }
  if (prefix === 'model') {
    return `${t('pages.admin.appRequests.entity.model', 'Model')}: ${value}`;
  }
  return entityId;
}

function ellipsised(text: string, title?: string): React.ReactElement {
  return (
    <Tooltip title={title ?? text} placement="top-start">
      <Typography
        variant="bodyMedium"
        color="text.secondary"
        sx={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
      >
        {text || '—'}
      </Typography>
    </Tooltip>
  );
}

export const AppRequestsTable = memo(function AppRequestsTable({
  requests,
  isLoading,
  sortField,
  sortDirection,
  onSort,
  onApprove,
  onOpenReject,
  pendingIds,
}: AppRequestsTableProps) {
  const theme = useTheme();

  const sortModel: GridSortModel = useMemo(
    () => (sortField ? [{ field: sortField, sort: sortDirection }] : []),
    [sortField, sortDirection],
  );

  const columns: GridColDef<AppRequestRow>[] = useMemo(
    () => [
      {
        field: 'issue_type',
        headerName: t('pages.admin.appRequests.column.application', 'Application'),
        flex: 1,
        minWidth: 150,
        sortable: true,
        renderCell: (params: GridRenderCellParams<AppRequestRow>) => (
          <Box sx={{ display: 'flex', flexDirection: 'column', justifyContent: 'center', minWidth: 0 }}>
            <Typography variant="bodyMedium" sx={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>
              {params.row.issue_type || '—'}
            </Typography>
            <Typography
              variant="bodySmall"
              color="text.secondary"
              sx={{ overflow: 'hidden', textOverflow: 'ellipsis' }}
            >
              {describeEntityId(params.row.entity_id) || '—'}
            </Typography>
          </Box>
        ),
      },
      {
        field: 'user_email',
        headerName: t('pages.admin.appRequests.column.user', 'Requesting User'),
        flex: 1,
        minWidth: 180,
        // Not on the server's sort allow-list — it is a joined column, not one of
        // this table's own — and an unknown `sort_by` there falls back to
        // `created_at`. A header that silently sorted by something else is worse
        // than one that does not sort.
        sortable: false,
        renderCell: (params: GridRenderCellParams<AppRequestRow>) =>
          ellipsised(params.row.user_email),
      },
      {
        field: 'project_id',
        headerName: t('pages.admin.appRequests.column.project', 'Project'),
        width: 100,
        sortable: true,
      },
      {
        field: 'description',
        headerName: t('pages.admin.appRequests.column.description', 'Description'),
        flex: 1.6,
        minWidth: 200,
        sortable: false,
        renderCell: (params: GridRenderCellParams<AppRequestRow>) => {
          const { description, rejection_comment: reason, created_project_id: projectId } = params.row;
          // A Project Request's approval (#871) causes something the six
          // other issue types never do: a real project. Surfacing the id
          // here is the only place an operator can see WHICH project one
          // of their approvals created, since nothing else on this page —
          // or the requester's own status read — names it otherwise.
          if (projectId !== undefined) {
            return (
              <Box sx={{ display: 'flex', flexDirection: 'column', justifyContent: 'center', minWidth: 0 }}>
                {ellipsised(description)}
                <Typography variant="bodySmall" color="success.main">
                  {t('pages.admin.appRequests.projectCreated', 'Project #{{id}} created', { id: projectId })}
                </Typography>
              </Box>
            );
          }
          if (reason === null || reason === '') return ellipsised(description);
          // A rejection reason has nowhere else to go: the reference page never
          // renders it back, so the operator's own words are invisible the
          // moment the dialog closes.
          return (
            <Box sx={{ display: 'flex', flexDirection: 'column', justifyContent: 'center', minWidth: 0 }}>
              {ellipsised(description)}
              <Tooltip title={reason} placement="top-start">
                <Typography
                  variant="bodySmall"
                  color="error.main"
                  sx={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                >
                  {t('pages.admin.appRequests.reasonPrefix', 'Reason:')} {reason}
                </Typography>
              </Tooltip>
            </Box>
          );
        },
      },
      {
        field: 'status',
        headerName: t('pages.admin.appRequests.column.status', 'Status'),
        width: 120,
        sortable: true,
        renderCell: (params: GridRenderCellParams<AppRequestRow>) => (
          <Chip
            size="small"
            variant="outlined"
            color={STATUS_COLOUR[params.row.status]}
            label={statusLabel(params.row.status)}
          />
        ),
      },
      {
        field: 'created_at',
        headerName: t('pages.admin.appRequests.column.requestedAt', 'Requested At'),
        width: 180,
        sortable: true,
        renderCell: (params: GridRenderCellParams<AppRequestRow>) => (
          <Typography variant="bodyMedium" color="text.secondary">
            {formatTimestamp(params.row.created_at)}
          </Typography>
        ),
      },
      {
        field: 'actions',
        headerName: t('pages.admin.appRequests.column.actions', 'Actions'),
        width: 110,
        sortable: false,
        disableColumnMenu: true,
        renderCell: (params: GridRenderCellParams<AppRequestRow>) => {
          const row = params.row;
          // A decided request has nothing left to decide. The server would
          // accept a second approval, but re-notifying the requester of a
          // decision they already have is not a control worth offering.
          if (row.status !== 'pending') return null;
          const busy = pendingIds.has(row.id);
          const approveLabel = t('pages.admin.appRequests.action.approve', 'Approve request');
          const rejectLabel = t('pages.admin.appRequests.action.reject', 'Reject request');
          return (
            <Box sx={{ display: 'flex', gap: 0.25 }}>
              {onApprove ? (
                <Tooltip title={approveLabel}>
                  <span>
                    <IconButton
                      size="small"
                      aria-label={`${approveLabel}: ${row.issue_type}`}
                      disabled={busy}
                      onClick={() => onApprove(row)}
                    >
                      <CheckCircleOutlineIcon fontSize="small" color="success" />
                    </IconButton>
                  </span>
                </Tooltip>
              ) : null}
              {onOpenReject ? (
                <Tooltip title={rejectLabel}>
                  <span>
                    <IconButton
                      size="small"
                      aria-label={`${rejectLabel}: ${row.issue_type}`}
                      disabled={busy}
                      onClick={() => onOpenReject(row)}
                    >
                      <CancelOutlinedIcon fontSize="small" color="error" />
                    </IconButton>
                  </span>
                </Tooltip>
              ) : null}
            </Box>
          );
        },
      },
    ],
    [onApprove, onOpenReject, pendingIds],
  );

  if (!isLoading && requests.length === 0) {
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
          {t('pages.admin.appRequests.empty', 'No app requests')}
        </Typography>
      </Box>
    );
  }

  return (
    <DataGrid
      rows={requests}
      columns={columns}
      loading={isLoading}
      rowHeight={56}
      hideFooter
      getRowId={(row: AppRequestRow) => row.id}
      sortingMode="server"
      sortModel={sortModel}
      onSortModelChange={(model: GridSortModel) => {
        const next = model[0];
        if (!next?.field) return;
        onSort(next.field, next.sort === 'desc' ? 'desc' : 'asc');
      }}
      sx={{ flex: 1, minHeight: 0, border: 'none' }}
    />
  );
});
