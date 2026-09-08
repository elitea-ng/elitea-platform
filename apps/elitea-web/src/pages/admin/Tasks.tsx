/**
 * Admin › Tasks — what is running on this platform, and the stop button.
 *
 * Reference (read-only): `frontends/admin_ui/frontend/src/pages/
 * SchedulesTasksPage/TasksTab.jsx` + `TasksTable.jsx`. Its column set (Task,
 * Task ID, User, Started, Status, actions), its status chips and its polling
 * are reproduced.
 *
 * ## Why this page exists next to Schedules & Tasks, rather than inside it
 *
 * That page's "Tasks" tab renders an unavailable notice, and it stays that way:
 * it is about pylon's Arbiter task node, which this platform does not have and
 * is not going to (`SchedulesTasks.tsx`'s header, and
 * `arbiterTaskNodeUnavailable` in the Go handler). This page answers the
 * QUESTION that tab asked, from this platform's own tables — a different
 * subject with a different route, so it gets its own nav entry rather than
 * quietly replacing a notice that is still true.
 *
 * ## Three differences from the reference
 *
 *  1. A KIND filter. The reference had one kind of task, so it had nothing to
 *     filter; here a row can be an index run, an agent turn, a toolkit tool
 *     call, a scheduled occurrence or an evaluation run.
 *  2. The cancel is offered PER ROW, from the server's own `cancellable` flag.
 *     The reference offered Stop on every row and let the server refuse. A
 *     control that answers 409 most of the time teaches an operator to ignore
 *     it.
 *  3. The poll is 10 seconds, not the reference's 5. The union is a heavier
 *     read than one in-process dict, and ten seconds is still well inside the
 *     window in which an operator watching a job notices it change.
 *
 * ## What is NOT here
 *
 * No start, and no log tail. Both are Arbiter operations on a registry of
 * plugin-declared maintenance tasks: there is nothing to start, and a job's
 * output is its own artefact rather than a task-node log stream.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminTasksTable } from './AdminTasksTable';
import { TASK_KINDS, type AdminTaskRow } from './api/adminTasksApi';
import { useAdminTasksPage } from './useAdminTasksPage';

/** The statuses the three sources use, offered as one flat filter list. */
const TASK_STATUSES = [
  'PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING', 'CANCELLING',
  'SUCCEEDED', 'FAILED', 'CANCELLED', 'COMPLETED', 'SUPERSEDED',
  'created', 'running', 'finished', 'errored', 'cancelled',
] as const;

function kindLabel(kind: string): string {
  return t(`pages.admin.tasks.kind.${kind}`, kind);
}

export interface AdminTasksProps {
  /**
   * The auto-refresh interval, in milliseconds, or `false` for no poll.
   *
   * The route passes nothing and gets the ten-second default. It is a PROP
   * rather than a constant so a test can turn the timer off: a 10-second timer
   * inside jsdom either makes the suite slow or makes it flaky, and the
   * alternative — mocking the hook — is the substitution R-M1 forbids, because
   * a test that replaces the hook stops proving the page is wired to it.
   */
  readonly refetchInterval?: number | false;
}

export function AdminTasks(props: AdminTasksProps = {}): ReactNode {
  const page = useAdminTasksPage(props.refetchInterval);
  const [pendingCancel, setPendingCancel] = useState<AdminTaskRow>();

  return (
    <DrawerPage>
      <Typography variant="headingMedium" sx={titleSx}>
        {t('pages.admin.tasks.title', 'Tasks')}
      </Typography>
      <Typography variant="bodyMedium" color="text.secondary" sx={subtitleSx}>
        {t(
          'pages.admin.tasks.subtitle',
          'Background jobs running on this platform. The list refreshes every ten seconds.',
        )}
      </Typography>

      <Box sx={filtersSx}>
        <Select
          size="small"
          displayEmpty
          value={page.kind}
          inputProps={{ 'aria-label': 'Kind' }}
          onChange={(event) => page.setKind(event.target.value)}
        >
          <MenuItem value="">{t('pages.admin.tasks.filter.allKinds', 'All kinds')}</MenuItem>
          {TASK_KINDS.map((kind) => (
            <MenuItem key={kind} value={kind}>{kindLabel(kind)}</MenuItem>
          ))}
        </Select>
        <Select
          size="small"
          displayEmpty
          value={page.status}
          inputProps={{ 'aria-label': 'Status' }}
          onChange={(event) => page.setStatus(event.target.value)}
        >
          <MenuItem value="">{t('pages.admin.tasks.filter.allStatuses', 'All statuses')}</MenuItem>
          {TASK_STATUSES.map((status) => (
            <MenuItem key={status} value={status}>{status}</MenuItem>
          ))}
        </Select>
      </Box>

      {page.isFetching && <LinearProgress data-testid="admin-tasks-loading" />}

      {page.unavailableReason !== undefined && (
        <Alert severity="warning" data-testid="admin-tasks-unavailable">
          {page.unavailableReason}
        </Alert>
      )}

      {page.errorMessage !== undefined && (
        <Alert severity="error" data-testid="admin-tasks-error">{page.errorMessage}</Alert>
      )}

      {/* The server's own words about its scan window, not a guess. A capped
          list that presents itself as the whole history is how an operator
          concludes a job is not running because they could not see it. */}
      {page.truncated && (
        <Alert severity="info" data-testid="admin-tasks-truncated">
          {t(
            'pages.admin.tasks.truncated',
            'Only the most recent jobs are listed. Narrow the filters to reach older ones.',
          )}
        </Alert>
      )}

      {page.unavailableReason === undefined && (
        <AdminTasksTable
          rows={page.rows}
          isCancelling={page.isCancelling}
          onCancel={setPendingCancel}
        />
      )}

      <Box sx={pagerSx}>
        <Typography variant="bodySmall" color="text.secondary" data-testid="admin-tasks-count">
          {t('pages.admin.tasks.count', 'Jobs')}
          {`: ${page.total}`}
        </Typography>
        <Button size="small" disabled={!page.hasPrevious} onClick={page.previousPage}>
          {t('common.previous', 'Previous')}
        </Button>
        <Button size="small" disabled={!page.hasNext} onClick={page.nextPage}>
          {t('common.next', 'Next')}
        </Button>
      </Box>

      <Dialog open={pendingCancel !== undefined} onClose={() => setPendingCancel(undefined)}>
        <DialogTitle>{t('pages.admin.tasks.cancelTitle', 'Stop this job?')}</DialogTitle>
        <DialogContent>
          {t(
            'pages.admin.tasks.cancelDescription',
            'The job is asked to stop. Work it has already done is kept.',
          )}
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setPendingCancel(undefined)}>{t('common.cancel', 'Cancel')}</Button>
          <Button
            color="error"
            variant="contained"
            onClick={() => {
              if (pendingCancel === undefined) return;
              page.cancel(pendingCancel);
              setPendingCancel(undefined);
            }}
          >
            {t('pages.admin.tasks.cancelConfirm', 'Stop job')}
          </Button>
        </DialogActions>
      </Dialog>
    </DrawerPage>
  );
}

const titleSx: SxProps<Theme> = (theme) => ({ marginBottom: theme.spacing(0.5) });
const subtitleSx: SxProps<Theme> = (theme) => ({ marginBottom: theme.spacing(2) });
const filtersSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  gap: theme.spacing(1.5),
  marginBottom: theme.spacing(2),
});
const pagerSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1.5),
  marginTop: theme.spacing(2),
});
