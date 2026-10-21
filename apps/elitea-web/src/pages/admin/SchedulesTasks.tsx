/**
 * Admin › Schedules & Tasks — unit A14, issue #200.
 *
 * Reference (read-only): `frontends/admin_ui/frontend/src/pages/SchedulesTasksPage/`.
 * Rewritten against this app's stack: Redux Toolkit → TanStack Query
 * (`./api/adminSchedulesApi`), axios → `eliteaFetch`, react-router → TanStack
 * Router (`./router`), MUI 7 → 9. State and handlers live in
 * `./useAdminSchedulesPage`.
 *
 * ## What this page schedules, and where that lives
 *
 * Rows of `centry.schedule`: a cron expression bound to the name of an internal
 * platform RPC. This is live and executing: `services/elitea-scheduler` polls
 * that exact table every minute and dispatches the due rows. It is also the
 * mechanism the indexing transition depends on — the `index_scheduling` row is
 * DISABLED in favour of elitea-main's own `index.schedule.scan.v1`, and
 * disabling a row is exactly this page's switch. Ported in full, with a real
 * read and a real write.
 *
 * The reference page also had "Tasks" and "Active Tasks" tabs over a plugin
 * runtime's in-process task registry. This platform has no such registry, so
 * those tabs are not ported. What they asked — what is running and can it be
 * stopped — is answered from this platform's own tables by Admin › Tasks
 * (`./Tasks.tsx`).
 *
 * Scheduled PIPELINE execution — the thing one might expect a page called
 * "Schedules & Tasks" to offer — is a fourth system again, and issue #193 records
 * that it has no home in the Go stack at all. Nothing on this page claims to
 * offer it, and no control here was invented to.
 *
 * ## The run-as identity of the one write
 *
 * A scheduled run has no interactive principal. The scheduler publishes
 * `rpc_func` onto the Arbiter bus fire-and-forget, and the handler at the other
 * end is an internal platform function with full privilege — there is no user to
 * run "as". That makes `rpc_func`/`rpc_kwargs` the security boundary of the
 * table rather than ordinary fields, so the write path accepts `name`, `cron`
 * and `active` only and REFUSES a body carrying either with a 400. A schedule's
 * timing can be changed here; what it invokes cannot.
 *
 * ## Authorisation
 *
 * `window.admin_ui_config.permissions` is presentation state and never a gate —
 * see `./adminUiConfig`. The read is gated server-side on
 * `configuration.scheduling.schedules.view` and the write on `…edit`, resolved
 * from `auth_core__user_role` per request.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import LinearProgress from '@mui/material/LinearProgress';
import Typography from '@mui/material/Typography';

import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { ScheduleHistoryDrawer } from './ScheduleHistoryDrawer';
import { SchedulesTable } from './SchedulesTable';
import { useAdminSchedulesPage, type AdminSchedulesPageState } from './useAdminSchedulesPage';


function errorText(message: string): string {
  return message === 'save'
    ? t('pages.admin.schedules.error.save', 'Failed to save the schedule.')
    : message;
}

function SchedulesBody({ state }: { readonly state: AdminSchedulesPageState }) {
  if (state.isError) {
    return (
      <Alert severity="warning" data-testid="admin-schedules-unavailable">
        {state.unavailableReason ??
          t('pages.admin.schedules.error.load', 'Failed to load schedules.')}
      </Alert>
    );
  }
  if (!state.rows) {
    return (
      <Typography variant="bodyMedium" color="text.secondary">
        {t('pages.admin.schedules.loading', 'Loading schedules…')}
      </Typography>
    );
  }
  return (
    <>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.schedules.description',
          'Background jobs that run on a cron timer. Each one calls an internal platform function; what a schedule calls is fixed and cannot be changed here. Click a name for its execution history.',
        )}
      </Typography>
      <SchedulesTable
        rows={state.rows}
        sort={state.sort}
        onSort={state.onSort}
        onOpenHistory={state.onOpenHistory}
        onToggleActive={state.onToggleActive}
        onCronChange={state.onCronChange}
        isSaving={state.isSaving}
      />
    </>
  );
}

export function AdminSchedulesTasks() {
  const state = useAdminSchedulesPage();

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }}>
      <Box
        sx={{
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'center',
          gap: '1rem',
          flexWrap: 'wrap',
        }}
      >
        <Typography variant="headingLarge" component="h1">
          {t('pages.admin.schedules.title', 'Schedules & Tasks')}
        </Typography>
        <SimpleSearchBar
          value={state.search}
          onChange={state.onSearchChange}
          placeholder={t('pages.admin.schedules.search', 'Search schedules')}
          data-testid="admin-schedules-search"
        />
      </Box>

      {state.isFetching ? <LinearProgress /> : null}

      {state.errorMessage !== '' ? (
        <Alert severity="error" onClose={state.onDismissError} data-testid="admin-schedules-error">
          {errorText(state.errorMessage)}
        </Alert>
      ) : null}
      {state.savedMessage !== '' ? (
        <Alert severity="success" onClose={state.onDismissSaved} data-testid="admin-schedules-saved">
          {t('pages.admin.schedules.saved', 'Schedule saved.')}
        </Alert>
      ) : null}

      <SchedulesBody state={state} />

      <ScheduleHistoryDrawer schedule={state.historySchedule} onClose={state.onCloseHistory} />
    </DrawerPage>
  );
}
