/**
 * Admin > Schedules (issue #1369).
 *
 * Lists the recurring routines the platform really runs - elitea-main's
 * scheduled jobs (index schedule scan, pipeline schedule scan, artifact
 * retention, token expiry notices) and elitea-scheduler's workers (price sync,
 * audit/sync/auth retention) - and lets an operator switch one off or give it
 * another schedule. A change reaches the owning process on its next tick or
 * poll; nothing restarts.
 *
 * It replaces the earlier table over `centry.schedule`, which nothing
 * executes. What a routine DOES is fixed in code and cannot be edited here;
 * only whether it runs, and when.
 *
 * ## Authorisation
 *
 * `window.admin_ui_config.permissions` is presentation state and never a gate
 * (see `./adminUiConfig`). The read is gated server-side on
 * `configuration.scheduling.schedules.view` and the write on `...edit`.
 *
 * "What is running right now, and can it be stopped" is Admin > Tasks
 * (`./Tasks.tsx`).
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import LinearProgress from '@mui/material/LinearProgress';
import Typography from '@mui/material/Typography';

import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

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
          'Recurring routines the platform runs. Switch one off, or give it another schedule; the change applies within a minute, without a restart. What a routine does is fixed and cannot be changed here.',
        )}
      </Typography>
      <SchedulesTable
        rows={state.rows}
        sort={state.sort}
        onSort={state.onSort}
        onToggleEnabled={state.onToggleEnabled}
        onScheduleChange={state.onScheduleChange}
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

    </DrawerPage>
  );
}
