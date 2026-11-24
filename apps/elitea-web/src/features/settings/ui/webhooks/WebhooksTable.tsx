/**
 * WebhooksTable — the project Webhooks list (#876): one row per outbound
 * webhook, with enable/disable, rotate and delete.
 *
 * The secret column says only whether a signing secret is set. The value is
 * write-only (issue 2963): no read returns it, so there is nothing to reveal
 * or copy here. Rotate is how a lost value is replaced.
 *
 * Deliberately a plain MUI `Table`, not a DataGrid: five columns, no
 * pagination, no sort — the same "no `GridTableContainer`" call
 * `TokensTable.tsx`'s header documents for the same reason.
 */
import { Fragment, useState } from 'react';

import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import KeyboardArrowDownIcon from '@mui/icons-material/KeyboardArrowDown';
import KeyboardArrowUpIcon from '@mui/icons-material/KeyboardArrowUp';
import RefreshIcon from '@mui/icons-material/Refresh';
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Collapse from '@mui/material/Collapse';
import IconButton from '@mui/material/IconButton';
import Paper from '@mui/material/Paper';
import Switch from '@mui/material/Switch';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { handleCopy } from '@/shared/lib/clipboard';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import type { WebhookPermissions } from '../../lib/webhooks/useWebhookPermissions';
import { MASKED_SECRET } from '../../lib/webhooks/webhookHelpers';
import { WebhookDeliveriesPanel } from './WebhookDeliveriesPanel';
import { monoFontFamily } from '@/shared/brand/typeScale';

export interface WebhookViewRow {
  readonly id: string;
  readonly url: string;
  readonly events: readonly string[];
  /** Whether deliveries are signed. The secret itself never reaches the client after it is shown once. */
  readonly hasSecret: boolean;
  readonly active: boolean;
}

export interface WebhooksTableProps {
  readonly projectId: string;
  readonly rows: readonly WebhookViewRow[];
  readonly isLoading: boolean;
  readonly permissions: WebhookPermissions;
  readonly onEdit: (row: WebhookViewRow) => void;
  readonly onRotate: (row: WebhookViewRow) => void;
  readonly onToggleActive: (row: WebhookViewRow, active: boolean) => void;
  readonly onDelete: (row: WebhookViewRow) => void;
}

const tableRootSx: SxProps<Theme> = { width: '100%' };
const emptyMessageSx: SxProps<Theme> = { padding: '1.5rem', textAlign: 'center', color: 'text.secondary' };
const urlCellSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.25rem', maxWidth: '20rem' };
const urlTextSx: SxProps<Theme> = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };
const secretCellSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.25rem', fontFamily: monoFontFamily };
const eventsCellSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.25rem', maxWidth: '16rem' };
const collapseCellSx: SxProps<Theme> = { paddingTop: 0, paddingBottom: 0, borderBottom: 'none' };

/** A set secret shows as a mask with a "set" label; an unset one says so. Never the value. */
function SecretCell({ row }: { readonly row: WebhookViewRow }) {
  return (
    <Box sx={secretCellSx} data-testid={`webhook-secret-${row.id}`}>
      {row.hasSecret ? (
        <Tooltip title={t('entities.webhook.table.secretWriteOnly', 'Shown once when created or rotated. Rotate to get a new one.')}>
          <Typography variant="bodySmall">{MASKED_SECRET}</Typography>
        </Tooltip>
      ) : (
        <Typography variant="bodySmall" color="text.secondary">
          {t('entities.webhook.table.secretNotSet', 'Not set')}
        </Typography>
      )}
    </Box>
  );
}

export function WebhooksTable({ projectId, rows, isLoading, permissions, onEdit, onRotate, onToggleActive, onDelete }: WebhooksTableProps) {
  const [pendingDelete, setPendingDelete] = useState<WebhookViewRow | null>(null);
  // Deliveries panels are collapsed by default — WebhookDeliveriesPanel only
  // fetches while its row is expanded, so a project with many webhooks does
  // not fire N delivery-list requests just for rendering the table.
  const [expandedId, setExpandedId] = useState<string | null>(null);

  if (!isLoading && rows.length === 0) {
    return (
      <Paper elevation={0} sx={emptyMessageSx} data-testid="webhooks-empty">
        <Typography variant="bodyMedium">
          {t('entities.webhook.table.empty', 'No webhooks yet. Register one to have {{product}} call your service when project events happen.')}
        </Typography>
      </Paper>
    );
  }

  return (
    <>
      <TableContainer component={Paper} elevation={0} sx={tableRootSx}>
        <Table size="small" data-testid="webhooks-table">
          <TableHead>
            <TableRow>
              <TableCell />
              <TableCell>{t('entities.webhook.table.url', 'URL')}</TableCell>
              <TableCell>{t('entities.webhook.table.events', 'Events')}</TableCell>
              <TableCell>{t('entities.webhook.table.secret', 'Secret')}</TableCell>
              <TableCell>{t('entities.webhook.table.active', 'Active')}</TableCell>
              <TableCell align="right">{t('entities.webhook.table.actions', 'Actions')}</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {rows.map((row) => {
              const isExpanded = expandedId === row.id;
              return (
              <Fragment key={row.id}>
              <TableRow data-testid={`webhook-row-${row.id}`}>
                <TableCell>
                  <Tooltip title={t('entities.webhook.table.deliveries', 'Recent deliveries')}>
                    <IconButton
                      size="small"
                      onClick={() => setExpandedId(isExpanded ? null : row.id)}
                      aria-label={t('entities.webhook.table.deliveriesAria', 'Toggle recent deliveries')}
                      data-testid={`webhook-expand-${row.id}`}
                    >
                      {isExpanded ? <KeyboardArrowUpIcon fontSize="small" /> : <KeyboardArrowDownIcon fontSize="small" />}
                    </IconButton>
                  </Tooltip>
                </TableCell>
                <TableCell>
                  <Box sx={urlCellSx}>
                    <Typography variant="bodySmall" sx={urlTextSx} title={row.url}>
                      {row.url}
                    </Typography>
                    <Tooltip title={t('entities.webhook.table.copyUrl', 'Copy URL')}>
                      <IconButton size="small" onClick={() => void handleCopy(row.url)} aria-label={t('entities.webhook.table.copyUrlAria', 'Copy URL')}>
                        <ContentCopyIcon fontSize="small" />
                      </IconButton>
                    </Tooltip>
                  </Box>
                </TableCell>
                <TableCell>
                  <Box sx={eventsCellSx}>
                    {row.events.length === 0
                      ? <Typography variant="bodySmall" color="text.secondary">{t('entities.webhook.table.allEvents', 'All events')}</Typography>
                      : row.events.map((event) => <Chip key={event} label={event} size="small" />)}
                  </Box>
                </TableCell>
                <TableCell>
                  <SecretCell row={row} />
                </TableCell>
                <TableCell>
                  <Switch
                    checked={row.active}
                    disabled={!permissions.canUpdate}
                    onChange={(event) => onToggleActive(row, event.target.checked)}
                    slotProps={{ input: { 'aria-label': t('entities.webhook.table.toggleActiveAria', 'Toggle active') } }}
                    data-testid={`webhook-active-toggle-${row.id}`}
                  />
                </TableCell>
                <TableCell align="right">
                  {permissions.canUpdate && (
                    <Tooltip title={t('entities.webhook.table.edit', 'Edit')}>
                      <IconButton size="small" onClick={() => onEdit(row)} aria-label={t('entities.webhook.table.editAria', 'Edit webhook')}>
                        <EditOutlinedIcon fontSize="small" />
                      </IconButton>
                    </Tooltip>
                  )}
                  {permissions.canUpdate && (
                    <Tooltip title={t('entities.webhook.table.rotate', 'Rotate secret')}>
                      <IconButton size="small" onClick={() => onRotate(row)} aria-label={t('entities.webhook.table.rotateAria', 'Rotate secret')} data-testid={`webhook-rotate-${row.id}`}>
                        <RefreshIcon fontSize="small" />
                      </IconButton>
                    </Tooltip>
                  )}
                  {permissions.canDelete && (
                    <Tooltip title={t('entities.webhook.table.delete', 'Delete')}>
                      <IconButton size="small" onClick={() => setPendingDelete(row)} aria-label={t('entities.webhook.table.deleteAria', 'Delete webhook')} data-testid={`webhook-delete-${row.id}`}>
                        <DeleteOutlineIcon fontSize="small" />
                      </IconButton>
                    </Tooltip>
                  )}
                </TableCell>
              </TableRow>
              <TableRow>
                <TableCell colSpan={6} sx={collapseCellSx}>
                  <Collapse in={isExpanded} unmountOnExit>
                    {isExpanded && (
                      <WebhookDeliveriesPanel projectId={projectId} webhookId={row.id} canRedeliver={permissions.canUpdate} />
                    )}
                  </Collapse>
                </TableCell>
              </TableRow>
              </Fragment>
              );
            })}
          </TableBody>
        </Table>
      </TableContainer>
      <DeleteEntityModal
        open={pendingDelete !== null}
        onClose={() => setPendingDelete(null)}
        onConfirm={() => {
          if (pendingDelete) onDelete(pendingDelete);
          setPendingDelete(null);
        }}
        name={pendingDelete?.url ?? ''}
        copy={{
          title: t('entities.webhook.dialog.deleteTitle', 'Delete webhook?'),
          textContent: t('entities.webhook.dialog.deleteTextContent', 'Are you sure you want to delete the webhook for '),
          confirmText: t('entities.webhook.dialog.deleteConfirm', 'Delete'),
          cancelText: t('entities.webhook.dialog.cancel', 'Cancel'),
        }}
        content={{ inline: t('entities.webhook.dialog.deleteInline', '? This action cannot be undone.') }}
        data-testid="webhook-delete-confirm-dialog"
      />
    </>
  );
}
