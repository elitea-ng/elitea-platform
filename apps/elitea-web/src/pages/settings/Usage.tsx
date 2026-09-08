/**
 * Settings › Usage — this project's current-period spend against its budget
 * (gap G13).
 *
 * READ-ONLY, and that is the surface's design rather than an unfinished half.
 * A project's ceiling is a platform decision: the write routes are gated on
 * `models.admin.project_budgets.edit`, which no project role carries. Admin ›
 * Budgets is where a limit is set. This page tells the people spending the
 * money where they stand.
 *
 * ## Three things here are ABSENT rather than zero
 *
 *  - `spend_available: false` means no accumulator row exists for this period.
 *    That is a fact about the write-back pipeline, not about the project, so it
 *    renders as "no data" and never as $0.00 — the second would claim the
 *    project made no billed calls.
 *  - `percent_used` is null for an unlimited project. There is no denominator,
 *    so there is no bar, and a 0% bar would imply a ceiling that exists.
 *  - `can_see_amounts: false` means the server REMOVED the five cost fields for
 *    this member. Percentages, the threshold and the period survive, so the
 *    usage bar still renders; the money simply is not there to show, and this
 *    page says which case it is in rather than rendering blanks.
 */
import { memo } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import LinearProgress from '@mui/material/LinearProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { useProjectUsage, type ProjectUsage } from './api/projectUsageApi';

const DASH = '—';

function money(value: number | null | undefined): string {
  if (value === null || value === undefined) return DASH;
  return `$${value.toFixed(2)}`;
}

/** The bar's colour: past the project's own warning threshold, then past 100%. */
function barColor(percent: number, warningPct: number): 'primary' | 'warning' | 'error' {
  if (percent >= 100) return 'error';
  if (percent >= warningPct) return 'warning';
  return 'primary';
}

function UsageFigure({ label, value }: { readonly label: string; readonly value: string }) {
  return (
    <Box sx={styles.figure}>
      <Typography variant="labelMedium" color="text.secondary">
        {label}
      </Typography>
      <Typography variant="h6">{value}</Typography>
    </Box>
  );
}

function UsageBody({ usage }: { readonly usage: ProjectUsage }) {
  const percentUsed = usage.percent_used;
  const warningPct = usage.warning_pct ?? 80;
  const unlimited = percentUsed === null || percentUsed === undefined;

  return (
    <>
      <Box sx={styles.figures}>
        <UsageFigure
          label={t('settings.usage.spend', 'Spent this period')}
          value={usage.spend_available === false ? DASH : money(usage.spend)}
        />
        <UsageFigure
          label={t('settings.usage.limit', 'Limit')}
          value={
            usage.effective_limit === null || usage.effective_limit === undefined
              ? t('settings.usage.unlimited', 'Unlimited')
              : money(usage.effective_limit)
          }
        />
        <UsageFigure
          label={t('settings.usage.remaining', 'Remaining')}
          value={money(usage.remaining)}
        />
      </Box>

      {unlimited ? (
        <Typography variant="bodyMedium" color="text.secondary">
          {t(
            'settings.usage.noCeiling',
            'This project has no enforced spend ceiling this period.',
          )}
        </Typography>
      ) : (
        <Box sx={styles.bar} data-testid="settings-usage-bar">
          <LinearProgress
            variant="determinate"
            value={Math.min(100, percentUsed)}
            color={barColor(percentUsed, warningPct)}
            sx={(theme) => ({ height: '0.5rem', borderRadius: theme.vars.shape.radiusSm })}
          />
          <Typography variant="bodySmall" color="text.secondary">
            {t('settings.usage.percent', '{{percent}}% of the limit used', {
              percent: percentUsed.toFixed(1),
            })}
          </Typography>
        </Box>
      )}

      {/* The server removed the money for this member. Saying so beats
          rendering five dashes that read as "nothing was spent". */}
      {usage.can_see_amounts === false ? (
        <Alert severity="info" data-testid="settings-usage-redacted">
          {t(
            'settings.usage.redacted',
            'Cost figures are visible to project administrators. Your usage percentage and period are shown above.',
          )}
        </Alert>
      ) : null}

      {usage.spend_available === false ? (
        <Alert severity="info" data-testid="settings-usage-no-data">
          {t(
            'settings.usage.noData',
            'No billed usage has been recorded for this period yet.',
          )}
        </Alert>
      ) : null}

      <Typography variant="bodySmall" color="text.secondary">
        {t('settings.usage.period', 'Period {{start}} to {{end}}', {
          start: usage.period_start ?? DASH,
          end: usage.period_end ?? DASH,
        })}
      </Typography>
    </>
  );
}

interface UsageProps {
  readonly projectId?: string | undefined;
}

const Usage = memo(({ projectId }: UsageProps) => {
  const query = useProjectUsage(projectId);

  return (
    <Box sx={styles.container} data-testid="settings-usage">
      {query.isLoading ? <LinearProgress data-testid="settings-usage-loading" /> : null}

      {/* Reported as the failure it is. An empty panel renders identically to
          "this project has spent nothing", which is a different claim (#130). */}
      {query.isError ? (
        <Alert severity="error" data-testid="settings-usage-error">
          {t('settings.usage.error', 'Failed to load usage for this project.')}
        </Alert>
      ) : null}

      {query.data !== undefined ? <UsageBody usage={query.data} /> : null}
    </Box>
  );
});

Usage.displayName = 'Usage';

export default Usage;

const styles: Record<string, SxProps<Theme>> = {
  container: {
    display: 'flex',
    flexDirection: 'column',
    gap: '1rem',
    padding: '1.5rem',
    width: '100%',
  },
  figures: { display: 'flex', gap: '2.5rem', flexWrap: 'wrap' },
  figure: { display: 'flex', flexDirection: 'column', gap: '0.25rem' },
  bar: { display: 'flex', flexDirection: 'column', gap: '0.375rem', maxWidth: '32rem' },
};
