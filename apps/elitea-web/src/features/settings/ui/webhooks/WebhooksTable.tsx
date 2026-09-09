/**
 * WebhooksTable — the project Webhooks list (#876): one row per outbound
 * webhook, with reveal/copy of the secret, enable/disable, rotate and
 * delete.
 *
 * Deliberately a plain MUI `Table`, not a DataGrid: five columns, no
 * pagination, no sort — the same "no `GridTableContainer`" call
 * `TokensTable.tsx`'s header documents for the same reason.
 */
import { useState } from 'react';

import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import RefreshIcon from '@mui/icons-material/Refresh';
import VisibilityIcon from '@mui/icons-material/Visibility';
import VisibilityOffIcon from '@mui/icons-material/VisibilityOff';
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
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

export interface WebhookViewRow {
  readonly id: string;
  readonly url: string;
  readonly events: readonly string[];
  readonly secret: string;
  readonly active: boolean;
}

export interface WebhooksTableProps {
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
const secretCellSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.25rem', fontFamily: 'monospace' };
const eventsCellSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.25rem', maxWidth: '16rem' };

/** One row's reveal state — local to the table, since it is purely a display toggle over data already fetched. */
function useRevealedSecrets() {
  const [revealed, setRevealed] = useState<ReadonlySet<string>>(new Set());
  const toggle = (id: string) =>
    setRevealed((previous) => {
      const next = new Set(previous);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  return { isRevealed: (id: string) => revealed.has(id), toggle };
}

interface SecretCellProps {
  readonly row: WebhookViewRow;
  readonly canReveal: boolean;
  readonly isRevealed: boolean;
  readonly onToggle: () => void;
}

function SecretCell({ row, canReveal, isRevealed, onToggle }: SecretCellProps) {
  const displayValue = isRevealed ? row.secret : MASKED_SECRET;
  return (
    <Box sx={secretCellSx}>
      <Typography variant="bodySmall2" data-testid={`webhook-secret-${row.id}`}>
        {displayValue}
      </Typography>
      {canReveal && (
        <Tooltip title={isRevealed ? t('entities.webhook.table.hideSecret', 'Hide secret') : t('entities.webhook.table.showSecret', 'Show secret')}>
          <IconButton size="small" onClick={onToggle} aria-label={t('entities.webhook.table.toggleSecretAria', 'Toggle secret visibility')}>
            {isRevealed ? <VisibilityOffIcon fontSize="small" /> : <VisibilityIcon fontSize="small" />}
          </IconButton>
        </Tooltip>
      )}
      <Tooltip title={t('entities.webhook.table.copySecret', 'Copy secret')}>
        <IconButton size="small" onClick={() => void handleCopy(row.secret)} aria-label={t('entities.webhook.table.copySecretAria', 'Copy secret')}>
          <ContentCopyIcon fontSize="small" />
        </IconButton>
      </Tooltip>
    </Box>
  );
}

export function WebhooksTable({ rows, isLoading, permissions, onEdit, onRotate, onToggleActive, onDelete }: WebhooksTableProps) {
  const { isRevealed, toggle } = useRevealedSecrets();
  const [pendingDelete, setPendingDelete] = useState<WebhookViewRow | null>(null);

  if (!isLoading && rows.length === 0) {
    return (
      <Paper elevation={0} sx={emptyMessageSx} data-testid="webhooks-empty">
        <Typography variant="bodyMedium">
          {t('entities.webhook.table.empty', 'No webhooks yet. Register one to have Elitea call your service when project events happen.')}
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
              <TableCell>{t('entities.webhook.table.url', 'URL')}</TableCell>
              <TableCell>{t('entities.webhook.table.events', 'Events')}</TableCell>
              <TableCell>{t('entities.webhook.table.secret', 'Secret')}</TableCell>
              <TableCell>{t('entities.webhook.table.active', 'Active')}</TableCell>
              <TableCell align="right">{t('entities.webhook.table.actions', 'Actions')}</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {rows.map((row) => (
              <TableRow key={row.id} data-testid={`webhook-row-${row.id}`}>
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
                      ? <Typography variant="bodySmall2" color="text.secondary">{t('entities.webhook.table.allEvents', 'All events')}</Typography>
                      : row.events.map((event) => <Chip key={event} label={event} size="small" />)}
                  </Box>
                </TableCell>
                <TableCell>
                  <SecretCell
                    row={row}
                    canReveal={permissions.canReveal}
                    isRevealed={isRevealed(row.id)}
                    onToggle={() => toggle(row.id)}
                  />
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
            ))}
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
