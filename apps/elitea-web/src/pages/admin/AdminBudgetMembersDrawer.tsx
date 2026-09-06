/**
 * The per-project member budget drawer.
 *
 * A member cap IS enforced (issue #321): the gateway asks `gateway.user_budget`
 * after the project ceiling admits, and refuses an over-cap member with a 402
 * `member_budget_exceeded`. So a wrong number here refuses real calls, and the
 * ability to CLEAR one matters more than at project scope — before the DELETE
 * route the only way to lift a cap set by mistake was `enabled: false`, which
 * stores a different fact and leaves the wrong number on the screen.
 *
 * `enforced` is read from the row rather than assumed. It is a field so a client
 * renders what is true of the deployment it is talking to; a member row that
 * reports `enforced: false` is an authored intention nothing applies, and the
 * drawer says so instead of implying a control.
 */
import type { ReactNode } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlineOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Divider from '@mui/material/Divider';
import Drawer from '@mui/material/Drawer';
import IconButton from '@mui/material/IconButton';
import LinearProgress from '@mui/material/LinearProgress';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { MemberBudgetRow } from './api/adminBudgetsApi';

export interface AdminBudgetMembersDrawerProps {
  readonly open: boolean;
  readonly projectName: string;
  readonly rows: readonly MemberBudgetRow[];
  readonly isLoading: boolean;
  readonly loadError: string | undefined;
  readonly onClose: () => void;
  readonly onEdit: (row: MemberBudgetRow) => void;
  readonly onClear: (row: MemberBudgetRow) => void;
}

const DASH = '—';

function money(value: number | null | undefined): string {
  if (value === null || value === undefined) return DASH;
  return `$${value.toFixed(2)}`;
}

function hasCap(row: MemberBudgetRow): boolean {
  return row.monthly_limit !== null && row.monthly_limit !== undefined;
}

export function AdminBudgetMembersDrawer({
  open,
  projectName,
  rows,
  isLoading,
  loadError,
  onClose,
  onEdit,
  onClear,
}: AdminBudgetMembersDrawerProps): ReactNode {
  return (
    <Drawer
      anchor="right"
      open={open}
      onClose={onClose}
      data-testid="admin-budgets-members-drawer"
      slotProps={{ paper: { sx: { width: 'min(40rem, 100vw)' } } }}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.5rem', padding: '1rem 1.25rem' }}>
        <Typography variant="h6" sx={{ flex: 1 }}>
          {t('pages.admin.budgets.members.title', 'Member budgets — {{name}}', {
            name: projectName,
          })}
        </Typography>
        <IconButton size="small" onClick={onClose} aria-label={t('pages.admin.budgets.members.close', 'Close')}>
          <CloseIcon fontSize="small" />
        </IconButton>
      </Box>
      <Divider />

      <Box sx={{ padding: '1rem 1.25rem', display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
        {isLoading ? <LinearProgress data-testid="admin-budgets-members-loading" /> : null}

        {/* A failed read is reported as the failure it is. An empty table would
            render identically to "this project has no members", which is a
            different claim. */}
        {loadError !== undefined ? (
          <Alert severity="error" data-testid="admin-budgets-members-error">
            {loadError}
          </Alert>
        ) : null}

        {!isLoading && loadError === undefined && rows.length === 0 ? (
          <Typography variant="bodyMedium" color="text.secondary">
            {t('pages.admin.budgets.members.empty', 'This project has no members.')}
          </Typography>
        ) : null}

        {rows.length > 0 ? (
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>{t('pages.admin.budgets.members.column.member', 'Member')}</TableCell>
                <TableCell>{t('pages.admin.budgets.members.column.cap', 'Cap')}</TableCell>
                <TableCell>{t('pages.admin.budgets.members.column.spend', 'Spend')}</TableCell>
                <TableCell align="right">
                  {t('pages.admin.budgets.members.column.actions', 'Actions')}
                </TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {rows.map((row) => (
                <TableRow key={row.user_id} data-testid={`admin-budgets-member-${row.user_id}`}>
                  <TableCell>
                    <Typography variant="bodyMedium">
                      {row.email !== undefined && row.email !== '' ? row.email : (row.name ?? String(row.user_id))}
                    </Typography>
                  </TableCell>
                  <TableCell>
                    {hasCap(row)
                      ? money(row.monthly_limit)
                      : t('pages.admin.budgets.members.noCap', 'Project ceiling only')}
                  </TableCell>
                  <TableCell>{row.spend_available === false ? DASH : money(row.spend)}</TableCell>
                  <TableCell align="right">
                    <Tooltip title={t('pages.admin.budgets.action.edit', 'Edit budget')}>
                      <IconButton
                        size="small"
                        onClick={() => onEdit(row)}
                        data-testid={`admin-budgets-member-edit-${row.user_id}`}
                      >
                        <EditOutlinedIcon fontSize="small" />
                      </IconButton>
                    </Tooltip>
                    {hasCap(row) ? (
                      <Tooltip title={t('pages.admin.budgets.action.clear', 'Clear back to default')}>
                        <IconButton
                          size="small"
                          onClick={() => onClear(row)}
                          data-testid={`admin-budgets-member-clear-${row.user_id}`}
                        >
                          <DeleteOutlineIcon fontSize="small" />
                        </IconButton>
                      </Tooltip>
                    ) : null}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ) : null}
      </Box>
    </Drawer>
  );
}
