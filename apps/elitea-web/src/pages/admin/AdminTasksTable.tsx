/**
 * The Admin › Tasks table.
 *
 * Column set from the reference (`admin_ui`'s `TasksTable.jsx:28-53`): Task,
 * Task ID, User, Started, Status, actions — plus Kind and Finished, which the
 * reference had no need for.
 *
 * ONE THING THE REFERENCE DOES THAT THIS DOES NOT: it derives the task name by
 * regex out of a stringified Python dict (`meta.match(/'task':\s*'([^']+)'/)`).
 * The server sends `name` as a field here, so there is nothing to parse.
 */
import type { ReactNode } from 'react';

import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import StopCircleOutlinedIcon from '@mui/icons-material/StopCircleOutlined';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { AdminTaskRow } from './api/adminTasksApi';

export interface AdminTasksTableProps {
  readonly rows: readonly AdminTaskRow[];
  readonly isCancelling: boolean;
  readonly onCancel: (row: AdminTaskRow) => void;
}

/**
 * The chip colour for a status.
 *
 * The three sources use three vocabularies (the runtime plane's upper-case
 * states, the scheduler's, and the evaluation lower-case ones), so this maps
 * them all rather than one. An unmapped status renders as `default` — visible
 * and uncoloured, which is the honest answer for a state this page has not been
 * taught about, and better than colouring it green by accident.
 */
function statusColour(status: string): 'default' | 'success' | 'error' | 'warning' {
  switch (status) {
    case 'RUNNING':
    case 'CLAIMED':
    case 'DISPATCHED':
    case 'SETTLING':
    case 'running':
      return 'success';
    case 'FAILED':
    case 'errored':
      return 'error';
    case 'CANCELLING':
    case 'CANCELLED':
    case 'cancelled':
      return 'warning';
    default:
      return 'default';
  }
}

/** An absent timestamp renders as a dash, never as an empty cell. */
function formatMoment(value: string | null): string {
  if (value === null || value === '') return '—';
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

export function AdminTasksTable(props: AdminTasksTableProps): ReactNode {
  if (props.rows.length === 0) {
    return (
      <Typography variant="bodyMedium" color="text.secondary" data-testid="admin-tasks-empty">
        {t('pages.admin.tasks.empty', 'No background jobs match these filters.')}
      </Typography>
    );
  }

  return (
    <TableContainer>
      <Table size="small" aria-label={t('pages.admin.tasks.tableLabel', 'Background jobs')}>
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.tasks.column.name', 'Task')}</TableCell>
            <TableCell>{t('pages.admin.tasks.column.kind', 'Kind')}</TableCell>
            <TableCell>{t('pages.admin.tasks.column.taskId', 'Task ID')}</TableCell>
            <TableCell>{t('pages.admin.tasks.column.user', 'User')}</TableCell>
            <TableCell>{t('pages.admin.tasks.column.started', 'Started')}</TableCell>
            <TableCell>{t('pages.admin.tasks.column.finished', 'Finished')}</TableCell>
            <TableCell>{t('pages.admin.tasks.column.status', 'Status')}</TableCell>
            <TableCell align="right" />
          </TableRow>
        </TableHead>
        <TableBody>
          {props.rows.map((row) => (
            <TableRow key={`${row.kind}:${row.task_id}`} data-testid={`admin-task-${row.task_id}`}>
              <TableCell>{row.name}</TableCell>
              <TableCell>{row.kind}</TableCell>
              <TableCell>{row.task_id}</TableCell>
              <TableCell>{row.user === '' ? '\u2014' : row.user}</TableCell>
              <TableCell>{formatMoment(row.started_at)}</TableCell>
              <TableCell>{formatMoment(row.finished_at)}</TableCell>
              <TableCell>
                <Chip size="small" label={row.status} color={statusColour(row.status)} />
              </TableCell>
              <TableCell align="right">
                {/* Rendered only where the server says the cancel would be
                    accepted — see this file's header, and the `cancellable`
                    flag the listing computes per row. */}
                {row.cancellable && (
                  <Tooltip title={t('pages.admin.tasks.stop', 'Stop job')}>
                    <span>
                      <IconButton
                        size="small"
                        color="tertiary"
                        disabled={props.isCancelling}
                        aria-label={`Stop ${row.task_id}`}
                        onClick={() => props.onCancel(row)}
                      >
                        <StopCircleOutlinedIcon fontSize="small" />
                      </IconButton>
                    </span>
                  </Tooltip>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}

