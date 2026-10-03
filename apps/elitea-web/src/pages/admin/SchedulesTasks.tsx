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
 * The reference page has three tabs and they are three DIFFERENT systems. This
 * matters more than the layout does, because only one of them survives the
 * replatform.
 *
 *  1. **Schedules** — rows of `centry.schedule`: a cron expression bound to the
 *     name of an internal platform RPC. This is live and executing:
 *     `services/elitea-scheduler` polls that exact table every minute and
 *     dispatches the due rows. It is also the mechanism the indexing transition
 *     depends on — `services/elitea-scheduler/RETIREMENT.md` records that the
 *     `index_scheduling` row is DISABLED in the hybrid deployment in favour of
 *     elitea-main's own `index.schedule.scan.v1`, and disabling a row is exactly
 *     this tab's switch. Ported in full, with a real read and a real write.
 *
 *  2. **Tasks** — one-off maintenance operations registered by Pylon PLUGINS.
 *     pylon's handler walks `self.module.context.module_manager.modules[…]` and
 *     starts, stops and tails them through an in-process Arbiter `task_node`
 *     (`legacy/plugins/admin/api/v2/tasks.py`).
 *
 *  3. **Active Tasks** — the same `task_node`'s `global_pool_state` and
 *     `global_task_state`, per Pylon node, with per-node refresh and stop
 *     (`legacy/plugins/admin/api/v2/active_tasks.py`).
 *
 * Tabs 2 and 3 are Pylon plugin loading and Arbiter runtime introspection, which
 * `AGENTS.md`'s architecture boundaries name explicitly as things the target
 * architecture does NOT preserve. There is no Go equivalent and no plan for one,
 * so there is nothing to port them onto. They render as an unavailable notice
 * carrying that reason — not as an empty table, and not as buttons that no-op.
 *
 * That is a correction, not a shortfall. `/admin/tasks` and `/admin/active_tasks`
 * previously answered 200 with an empty collection, so "nothing is running" and
 * "this deployment cannot see what is running" rendered identically; an operator
 * reading the former during an incident concludes the platform is idle. Both now
 * answer 501 with the same sentence shown below.
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
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import Typography from '@mui/material/Typography';

import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { ScheduleHistoryDrawer } from './ScheduleHistoryDrawer';
import { SchedulesTable } from './SchedulesTable';
import { useAdminSchedulesPage, type AdminSchedulesPageState } from './useAdminSchedulesPage';


/**
 * Applied to each `Tab` rather than through a `& .MuiTab-root` selector on the
 * `Tabs` container: R-T6 keeps MUI internals out of call sites.
 */
const TAB_SX = { textTransform: 'none', minHeight: '2.5rem' } as const;

/**
 * Hoisted so the page and its tests cannot drift apart on the wording, and so
 * the reason travels with the control rather than living in a comment.
 */
//
// Written as ONE string literal rather than a readable concatenation because
// the i18n extractor reads the fallback as source text and reports `'a' + 'b'`
// as an interpolated fallback it cannot resolve.
const TASK_NODES_UNAVAILABLE = t(
  'pages.admin.schedules.taskNodesUnavailable',
  'Admin tasks run inside the Pylon plugin runtime and are driven through the Arbiter task node. That runtime is not part of this platform, so this deployment cannot list, start or stop them. Use the Pylon admin panel while the hybrid deployment is running.',
);

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
        {/* The search box filters schedules only, so it is not rendered on the
            tabs it could not affect — a control that does nothing on the tab it
            is drawn over reads as broken. */}
        {state.activeTab === 'schedules' ? (
          <SimpleSearchBar
            value={state.search}
            onChange={state.onSearchChange}
            placeholder={t('pages.admin.schedules.search', 'Search schedules')}
            data-testid="admin-schedules-search"
          />
        ) : null}
      </Box>

      <Tabs
        value={state.activeTab}
        onChange={(_event, next: AdminSchedulesPageState['activeTab']) => state.onTabChange(next)}
        sx={{ minHeight: '2.5rem' }}
      >
        <Tab
          value="schedules"
          sx={TAB_SX}
          label={t('pages.admin.schedules.tab.schedules', 'Schedules')}
        />
        <Tab value="tasks" sx={TAB_SX} label={t('pages.admin.schedules.tab.tasks', 'Tasks')} />
        <Tab
          value="active-tasks"
          sx={TAB_SX}
          label={t('pages.admin.schedules.tab.activeTasks', 'Active Tasks')}
        />
      </Tabs>

      {state.isFetching && state.activeTab === 'schedules' ? <LinearProgress /> : null}

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

      {state.activeTab === 'schedules' ? (
        <SchedulesBody state={state} />
      ) : (
        <Alert severity="info" data-testid="admin-task-nodes-unavailable">
          {TASK_NODES_UNAVAILABLE}
        </Alert>
      )}

      <ScheduleHistoryDrawer schedule={state.historySchedule} onClose={state.onCloseHistory} />
    </DrawerPage>
  );
}
