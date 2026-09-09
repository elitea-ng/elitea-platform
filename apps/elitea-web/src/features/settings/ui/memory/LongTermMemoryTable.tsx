/**
 * LongTermMemoryTable — the list half of Settings > Memory's "Long-term
 * Memory" panel (#870): one row per remembered fact, with its tags, an
 * enable/disable Switch, and edit/delete actions.
 *
 * Same "plain MUI Table, no DataGrid" call `WebhooksTable.tsx` makes for the
 * same reason: a handful of columns, no pagination, no sort.
 */
import { useState } from 'react';

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
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

export interface LongTermMemoryViewRow {
  readonly id: string;
  readonly content: string;
  readonly tags: readonly string[];
  readonly enabled: boolean;
}

export interface LongTermMemoryTableProps {
  readonly rows: readonly LongTermMemoryViewRow[];
  readonly isLoading: boolean;
  readonly canWrite: boolean;
  readonly onEdit: (row: LongTermMemoryViewRow) => void;
  readonly onToggleEnabled: (row: LongTermMemoryViewRow, enabled: boolean) => void;
  readonly onDelete: (row: LongTermMemoryViewRow) => void;
}

const tableRootSx: SxProps<Theme> = { width: '100%' };
const emptyMessageSx: SxProps<Theme> = { padding: '1.5rem', textAlign: 'center', color: 'text.secondary' };
const contentCellSx: SxProps<Theme> = { maxWidth: '28rem', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' };
const tagsCellSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.25rem', maxWidth: '14rem' };

function summarize(content: string): string {
  const singleLine = content.replace(/\s+/g, ' ').trim();
  return singleLine.length > 160 ? `${singleLine.slice(0, 160)}…` : singleLine;
}

export function LongTermMemoryTable({ rows, isLoading, canWrite, onEdit, onToggleEnabled, onDelete }: LongTermMemoryTableProps) {
  const [pendingDelete, setPendingDelete] = useState<LongTermMemoryViewRow | null>(null);

  if (!isLoading && rows.length === 0) {
    return (
      <Paper elevation={0} sx={emptyMessageSx} data-testid="long-term-memory-empty">
        <Typography variant="bodyMedium">
          {t(
            'settings.longTermMemory.table.empty',
            'No memories yet. Save something for Elitea to remember across your conversations.',
          )}
        </Typography>
      </Paper>
    );
  }

  return (
    <>
      <TableContainer component={Paper} elevation={0} sx={tableRootSx}>
        <Table size="small" data-testid="long-term-memory-table">
          <TableHead>
            <TableRow>
              <TableCell>{t('settings.longTermMemory.table.content', 'Memory')}</TableCell>
              <TableCell>{t('settings.longTermMemory.table.tags', 'Tags')}</TableCell>
              <TableCell>{t('settings.longTermMemory.table.enabled', 'Enabled')}</TableCell>
              <TableCell align="right">{t('settings.longTermMemory.table.actions', 'Actions')}</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {rows.map((row) => (
              <TableRow key={row.id} data-testid={`long-term-memory-row-${row.id}`}>
                <TableCell sx={contentCellSx}>
                  <Typography variant="bodySmall" data-testid={`long-term-memory-content-${row.id}`}>
                    {summarize(row.content)}
                  </Typography>
                </TableCell>
                <TableCell>
                  <Box sx={tagsCellSx}>
                    {row.tags.map((tag) => (
                      <Chip key={tag} label={tag} size="small" />
                    ))}
                  </Box>
                </TableCell>
                <TableCell>
                  <Switch
                    checked={row.enabled}
                    disabled={!canWrite}
                    onChange={(event) => onToggleEnabled(row, event.target.checked)}
                    slotProps={{ input: { 'aria-label': t('settings.longTermMemory.table.toggleEnabledAria', 'Toggle memory enabled') } }}
                    data-testid={`long-term-memory-enabled-toggle-${row.id}`}
                  />
                </TableCell>
                <TableCell align="right">
                  {canWrite && (
                    <Tooltip title={t('settings.longTermMemory.table.edit', 'Edit')}>
                      <IconButton
                        size="small"
                        onClick={() => onEdit(row)}
                        aria-label={t('settings.longTermMemory.table.editAria', 'Edit memory')}
                        data-testid={`long-term-memory-edit-${row.id}`}
                      >
                        <EditOutlinedIcon fontSize="small" />
                      </IconButton>
                    </Tooltip>
                  )}
                  {canWrite && (
                    <Tooltip title={t('settings.longTermMemory.table.delete', 'Delete')}>
                      <IconButton
                        size="small"
                        onClick={() => setPendingDelete(row)}
                        aria-label={t('settings.longTermMemory.table.deleteAria', 'Delete memory')}
                        data-testid={`long-term-memory-delete-${row.id}`}
                      >
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
        name={pendingDelete ? summarize(pendingDelete.content) : ''}
        copy={{
          title: t('settings.longTermMemory.dialog.deleteTitle', 'Delete memory?'),
          textContent: t('settings.longTermMemory.dialog.deleteTextContent', 'Are you sure you want to delete '),
          confirmText: t('settings.longTermMemory.dialog.deleteConfirm', 'Delete'),
          cancelText: t('settings.longTermMemory.dialog.cancel', 'Cancel'),
        }}
        content={{ inline: t('settings.longTermMemory.dialog.deleteInline', '? This action cannot be undone.') }}
        data-testid="long-term-memory-delete-confirm-dialog"
      />
    </>
  );
}
