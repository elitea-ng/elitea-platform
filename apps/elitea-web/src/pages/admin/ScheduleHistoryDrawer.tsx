/**
 * A schedule's execution history (unit A14, issue #200).
 *
 * ## This is reuse, not a new read
 *
 * A schedule execution IS an audit event: the pylon scheduler opens a
 * `Schedule: {name} -> {rpc_func}` span with `telemetry.data_type =
 * schedule_execution` (`legacy/plugins/scheduling/models/schedule.py`), and the
 * tracing plugin writes it to `centry.audit_events` with `event_type =
 * 'schedule'` — which `./api/adminAuditApi` already names in its
 * `SYSTEM_EVENT_TYPES`.
 *
 * So the whole drawer runs on `useAuditSpans` from the Audit Trail port,
 * unchanged, and this unit adds no endpoint for it. The reference page reached
 * the same conclusion and queried its own audit-trail client the same way.
 *
 * ## The `search` filter is doing load-bearing work
 *
 * The server has no "events for schedule N" filter, so the span for THIS
 * schedule is selected by matching the span name the scheduler writes. That is
 * a substring match over a free-text column, which is why the drawer says
 * "matching" rather than presenting the count as authoritative: a schedule whose
 * name is a prefix of another's would draw both. Stating that beats a number
 * that quietly over-counts.
 */
import { useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Drawer from '@mui/material/Drawer';
import IconButton from '@mui/material/IconButton';
import LinearProgress from '@mui/material/LinearProgress';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';
import CloseIcon from '@mui/icons-material/Close';

import { t } from '@/shared/i18n';

import { formatDuration } from './auditFormat';
import { useAuditSpans, type AuditSpanRow } from './api/adminAuditApi';
import type { AdminScheduleRow } from './api/adminSchedulesApi';
import { monoFontFamily } from '@/shared/brand/typeScale';

const PAGE_SIZE = 50;
const HISTORY_WINDOW_DAYS = 7;

export interface ScheduleHistoryDrawerProps {
  readonly schedule: AdminScheduleRow | null;
  readonly onClose: () => void;
}

function windowBounds(): { readonly dateFrom: string; readonly dateTo: string } {
  const to = new Date();
  const from = new Date(to);
  from.setDate(from.getDate() - HISTORY_WINDOW_DAYS);
  return { dateFrom: from.toISOString(), dateTo: to.toISOString() };
}

function ExecutionRow({ row }: { readonly row: AuditSpanRow }) {
  return (
    <Stack
      direction="row"
      spacing={1}
      sx={{ py: '0.5rem', alignItems: 'center', justifyContent: 'space-between' }}
    >
      <Typography variant="bodySmall" color="text.secondary">
        {row.timestamp === null ? '—' : new Date(row.timestamp).toLocaleString()}
      </Typography>
      <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
        <Typography variant="bodySmall" color="text.secondary">
          {formatDuration(row.duration_ms)}
        </Typography>
        <Chip
          size="small"
          variant="outlined"
          color={row.is_error ? 'error' : 'success'}
          label={
            row.is_error
              ? t('pages.admin.schedules.history.failed', 'Failed')
              : t('pages.admin.schedules.history.succeeded', 'Succeeded')
          }
        />
      </Stack>
    </Stack>
  );
}

export function ScheduleHistoryDrawer({ schedule, onClose }: ScheduleHistoryDrawerProps) {
  // Frozen for the drawer's lifetime, not recomputed per render: a window that
  // slides on every render is a new query key every render, which refetches for
  // ever.
  const [bounds] = useState(windowBounds);

  const params = useMemo(
    () => ({
      limit: PAGE_SIZE,
      offset: 0,
      sortBy: 'timestamp',
      sortOrder: 'desc' as const,
      eventTypes: 'schedule',
      search: schedule === null ? undefined : `Schedule: ${schedule.name}`,
      ...bounds,
    }),
    [schedule, bounds],
  );

  const spans = useAuditSpans(params, { enabled: schedule !== null });
  const rows = spans.data?.rows ?? [];

  return (
    <Drawer
      anchor="right"
      open={schedule !== null}
      onClose={onClose}
      slotProps={{ paper: { sx: { width: '32rem', maxWidth: '90vw', p: '1rem' } } }}
    >
      <Stack direction="row" sx={{ justifyContent: 'space-between', alignItems: 'flex-start' }}>
        <Box>
          <Typography variant="headingSmall" component="h2">
            {t('pages.admin.schedules.history.title', 'Execution history')}
          </Typography>
          <Typography variant="bodySmall" color="text.secondary">
            {schedule?.name}
          </Typography>
        </Box>
        <IconButton
          size="small"
          onClick={onClose}
          aria-label={t('pages.admin.schedules.history.close', 'Close execution history')}
        >
          <CloseIcon fontSize="small" />
        </IconButton>
      </Stack>

      {schedule !== null ? (
        <Stack direction="row" spacing={0.5} sx={{ mt: '0.75rem', flexWrap: 'wrap', gap: '0.25rem' }}>
          <Chip size="small" variant="outlined" label={schedule.cron} sx={{ fontFamily: monoFontFamily }} />
          <Chip size="small" variant="outlined" label={schedule.rpc_func} sx={{ fontFamily: monoFontFamily }} />
          <Chip
            size="small"
            color={schedule.active ? 'success' : 'default'}
            label={
              schedule.active
                ? t('pages.admin.schedules.status.active', 'Active')
                : t('pages.admin.schedules.status.inactive', 'Inactive')
            }
          />
        </Stack>
      ) : null}

      <Typography variant="bodySmall" color="text.secondary" sx={{ mt: '0.75rem' }}>
        {t(
          'pages.admin.schedules.history.window',
          'Audit events of type "schedule" matching this schedule in the last {{days}} days.',
          { days: HISTORY_WINDOW_DAYS },
        )}
      </Typography>

      {spans.isFetching ? <LinearProgress sx={{ mt: '0.5rem' }} /> : null}

      {spans.isError ? (
        <Alert severity="warning" sx={{ mt: '0.75rem' }} data-testid="admin-schedule-history-error">
          {t('pages.admin.schedules.history.error', 'Failed to load execution history.')}
        </Alert>
      ) : null}

      {!spans.isError && !spans.isFetching && rows.length === 0 ? (
        <Typography
          variant="bodyMedium"
          color="text.secondary"
          sx={{ mt: '0.75rem' }}
          data-testid="admin-schedule-history-empty"
        >
          {t('pages.admin.schedules.history.empty', 'No executions recorded in this period.')}
        </Typography>
      ) : null}

      <Box sx={{ mt: '0.5rem', overflowY: 'auto' }}>
        {rows.map((row) => (
          <ExecutionRow key={row.id} row={row} />
        ))}
      </Box>
    </Drawer>
  );
}
