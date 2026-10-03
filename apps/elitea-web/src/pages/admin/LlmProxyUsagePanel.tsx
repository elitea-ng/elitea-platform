/**
 * Admin › Configuration › LLM Proxy — the Usage report.
 *
 * ## The screen this restores
 *
 * LiteLLM's admin UI had a Usage page — spend over time, a per-model table,
 * per-key and per-team breakdowns, token and request counts — and ADR-0015
 * removed LiteLLM without replacing it. Migration 0084's own header records what
 * that cost: the budget accumulator holds money per (scope, scope_id, period)
 * and nothing else, so "the port of that page has a meter and nothing else".
 * `gateway.llm_usage_events` is the per-request ledger 0084 added, and this is
 * the platform-wide reading of it.
 *
 * The product already has a PROJECT-scoped usage view (Settings → Usage). What
 * had no home anywhere was the operator's question — what is this deployment
 * spending in total, on which providers and models, and who accounts for it —
 * which previously could only be answered by opening every project in turn.
 *
 * ## Every number here is a report, and none of them is a control
 *
 * No budget decision reads this ledger and nothing on this screen writes. The
 * counters that ENFORCE budgets are `gateway.llm_budget_accumulators`, authored
 * on `/admin/app/governance`; summing the two together would double-count, and
 * this panel never presents one as the other.
 *
 * ## Why each section carries its own failure
 *
 * A breakdown that failed to load renders exactly like a breakdown with nothing
 * in it, and "nothing was spent" is the reassuring reading. The server answers
 * with a per-section error for that reason and this panel shows each one where
 * its table would have been, rather than collapsing them into a single banner
 * that would leave three working tables looking broken.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import LinearProgress from '@mui/material/LinearProgress';
import Paper from '@mui/material/Paper';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { UsageWindowSelect } from './LlmProxyModelsPanel';
import {
  useAdminLlmUsage,
  type LlmUsageDay,
  type LlmUsageSlice,
  type LlmUsageTotals,
} from './api/adminLlmUsageApi';
import type { UsageWindow } from './api/adminLlmProxyApi';

/**
 * Money, at a precision that separates "billed something" from "billed nothing".
 *
 * Same rule as the catalogue's `costLabel`, and it exists for the same reason:
 * an uncatalogued audio model bills zero and no budget can stop it, so rounding
 * a real sub-cent spend to `$0.0000` would hide the one distinction this surface
 * is here to make visible.
 */
function costLabel(value: number): string {
  if (value > 0 && value < 0.0001) return '<$0.0001';
  return `$${value.toFixed(4)}`;
}

/** Thousands separators, so a seven-digit token count is readable at a glance. */
function countLabel(value: number): string {
  return value.toLocaleString('en-US');
}

/** One headline figure. */
function StatTile({ label, value, hint }: {
  readonly label: string;
  readonly value: string;
  readonly hint?: string;
}): ReactNode {
  return (
    <Paper
      variant="outlined"
      sx={{ padding: '0.75rem 1rem', minWidth: '10rem', flex: '1 1 10rem' }}
    >
      <Typography variant="bodySmall" color="text.secondary">
        {label}
      </Typography>
      <Typography variant="headingSmall" component="h6">
        {value}
      </Typography>
      {hint !== undefined ? (
        <Typography variant="bodySmall" color="text.secondary">
          {hint}
        </Typography>
      ) : null}
    </Paper>
  );
}

function UsageTotalsRow({ totals }: { readonly totals: LlmUsageTotals }): ReactNode {
  return (
    <Box sx={{ display: 'flex', gap: '0.75rem', flexWrap: 'wrap' }} data-testid="llm-proxy-usage-totals">
      <StatTile
        label={t('pages.admin.llmProxy.usage.spend', 'Spend')}
        value={costLabel(totals.cost_usd)}
      />
      <StatTile
        label={t('pages.admin.llmProxy.usage.requests', 'Requests')}
        value={countLabel(totals.requests)}
      />
      <StatTile
        label={t('pages.admin.llmProxy.usage.tokens', 'Tokens')}
        value={countLabel(totals.total_tokens)}
        // The split is the hint rather than two more tiles: prompt and
        // completion tokens are almost always read as a ratio of the total, not
        // as figures in their own right.
        hint={t('pages.admin.llmProxy.usage.tokenSplit', 'in {{prompt}} / out {{completion}}', {
          prompt: countLabel(totals.prompt_tokens),
          completion: countLabel(totals.completion_tokens),
        })}
      />
      <StatTile
        label={t('pages.admin.llmProxy.usage.models', 'Models called')}
        value={countLabel(totals.models)}
      />
      <StatTile
        label={t('pages.admin.llmProxy.usage.projects', 'Projects')}
        value={countLabel(totals.projects)}
      />
    </Box>
  );
}

/**
 * The per-day series, as a table rather than a chart.
 *
 * A chart would need a plotting dependency this app does not carry, and the
 * question an operator brings to a daily series here — which day was expensive,
 * and how much — is answered exactly by the numbers. A shape that only conveys
 * a trend would be a downgrade for the auditing use this panel serves.
 */
function UsageSeries({ daily }: { readonly daily: readonly LlmUsageDay[] }): ReactNode {
  return (
    <TableContainer component={Paper} variant="outlined">
      <Table size="small" data-testid="llm-proxy-usage-daily">
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.llmProxy.usage.day', 'Day (UTC)')}</TableCell>
            <TableCell align="right">{t('pages.admin.llmProxy.usage.requests', 'Requests')}</TableCell>
            <TableCell align="right">{t('pages.admin.llmProxy.usage.tokens', 'Tokens')}</TableCell>
            <TableCell align="right">{t('pages.admin.llmProxy.usage.spend', 'Spend')}</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {daily.map((day) => (
            <TableRow key={day.day}>
              <TableCell>{day.day}</TableCell>
              <TableCell align="right">{countLabel(day.requests)}</TableCell>
              <TableCell align="right">{countLabel(day.total_tokens)}</TableCell>
              <TableCell align="right">{costLabel(day.cost_usd)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}

/**
 * One breakdown table, with its own failure, its own empty state and its own
 * truncation notice.
 *
 * The three breakdowns differ only in their heading and the meaning of the
 * secondary column, so they share this component: three copies would be three
 * places for the "a failed section renders as an empty one" mistake to come
 * back.
 */
function UsageBreakdown({
  title,
  detailHeading,
  rows,
  truncated,
  error,
  testId,
}: {
  readonly title: string;
  readonly detailHeading: string;
  readonly rows: readonly LlmUsageSlice[];
  readonly truncated: boolean;
  readonly error: string | undefined;
  readonly testId: string;
}): ReactNode {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem', flex: '1 1 22rem' }}>
      <Typography variant="bodyMedium" sx={{ fontWeight: 600 }}>
        {title}
      </Typography>

      {/* Stated where the table would be. A breakdown that FAILED and one with
          no spend in it look identical otherwise, and the second is the
          reassuring reading an operator would take. */}
      {error !== undefined ? (
        <Alert severity="warning" data-testid={`${testId}-error`}>
          {error}
        </Alert>
      ) : null}

      {error === undefined && rows.length === 0 ? (
        <Typography variant="bodySmall" color="text.secondary" data-testid={`${testId}-empty`}>
          {t('pages.admin.llmProxy.usage.noRows', 'No billed requests in this window.')}
        </Typography>
      ) : null}

      {rows.length > 0 ? (
        <TableContainer component={Paper} variant="outlined">
          <Table size="small" data-testid={testId}>
            <TableHead>
              <TableRow>
                <TableCell>{title}</TableCell>
                <TableCell>{detailHeading}</TableCell>
                <TableCell align="right">
                  {t('pages.admin.llmProxy.usage.requests', 'Requests')}
                </TableCell>
                <TableCell align="right">
                  {t('pages.admin.llmProxy.usage.tokens', 'Tokens')}
                </TableCell>
                <TableCell align="right">{t('pages.admin.llmProxy.usage.spend', 'Spend')}</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {rows.map((row) => (
                <TableRow key={row.key}>
                  <TableCell>{row.label}</TableCell>
                  <TableCell>
                    <Typography variant="bodySmall" color="text.secondary">
                      {row.detail}
                    </Typography>
                  </TableCell>
                  <TableCell align="right">{countLabel(row.requests)}</TableCell>
                  <TableCell align="right">{countLabel(row.total_tokens)}</TableCell>
                  <TableCell align="right">{costLabel(row.cost_usd)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableContainer>
      ) : null}

      {/* A capped list says so, for the reason the catalogue's does: a short
          list that looks complete is how an operator concludes a project is not
          spending anything when it is merely past the cap. */}
      {truncated ? (
        <Typography variant="bodySmall" color="text.secondary" data-testid={`${testId}-truncated`}>
          {t('pages.admin.llmProxy.usage.truncated', 'Showing the highest-spending rows only.')}
        </Typography>
      ) : null}
    </Box>
  );
}

export function LlmProxyUsagePanel(): ReactNode {
  // `usageWindow`, not `window`: the latter shadows the DOM global for the whole
  // of this function — the same naming rule `ModelsTab` states.
  const [usageWindow, setUsageWindow] = useState<UsageWindow>('24h');
  const { data, isPending, error } = useAdminLlmUsage(usageWindow);

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.llmProxy.usage.intro',
          'Every billed LLM request across the platform. This is a report over the usage ledger, not the counters that enforce budgets — budgets are authored under LLM Governance.',
        )}
      </Typography>

      <UsageWindowSelect usageWindow={usageWindow} onChange={setUsageWindow} />

      {/* The transport failure and the server's own refusal are separate
          statements. A 403 on the route and "the totals query timed out" call
          for different actions, and one alert saying "failed to load" would
          name neither. */}
      {error != null ? (
        <Alert severity="warning" data-testid="llm-proxy-usage-load-error">
          {t('pages.admin.llmProxy.usage.loadError', 'Failed to load the usage report.')}
        </Alert>
      ) : null}
      {data?.error !== undefined ? (
        <Alert severity="warning" data-testid="llm-proxy-usage-error">
          {data.error}
        </Alert>
      ) : null}

      {isPending ? (
        <LinearProgress aria-label={t('pages.admin.llmProxy.usage.loading', 'Loading usage')} />
      ) : null}

      {data !== undefined ? (
        <>
          <UsageTotalsRow totals={data.totals} />

          <Typography variant="bodyMedium" sx={{ fontWeight: 600 }}>
            {t('pages.admin.llmProxy.usage.byDay', 'By day')}
          </Typography>
          {data.daily_error !== undefined ? (
            <Alert severity="warning" data-testid="llm-proxy-usage-daily-error">
              {data.daily_error}
            </Alert>
          ) : null}
          {data.daily_error === undefined && data.daily.length === 0 ? (
            <Typography
              variant="bodySmall"
              color="text.secondary"
              data-testid="llm-proxy-usage-daily-empty"
            >
              {t('pages.admin.llmProxy.usage.noRows', 'No billed requests in this window.')}
            </Typography>
          ) : null}
          {data.daily.length > 0 ? <UsageSeries daily={data.daily} /> : null}

          <Box sx={{ display: 'flex', gap: '1.5rem', flexWrap: 'wrap', alignItems: 'flex-start' }}>
            <UsageBreakdown
              title={t('pages.admin.llmProxy.usage.byModel', 'By model')}
              detailHeading={t('pages.admin.llmProxy.usage.provider', 'Provider')}
              rows={data.models}
              truncated={data.models_truncated}
              error={data.models_error}
              testId="llm-proxy-usage-models"
            />
            <UsageBreakdown
              title={t('pages.admin.llmProxy.usage.byProject', 'By project')}
              detailHeading={t('pages.admin.llmProxy.usage.projectId', 'Project ID')}
              rows={data.projects}
              truncated={data.projects_truncated}
              error={data.projects_error}
              testId="llm-proxy-usage-projects"
            />
            <UsageBreakdown
              title={t('pages.admin.llmProxy.usage.byMember', 'By member')}
              detailHeading={t('pages.admin.llmProxy.usage.userId', 'User ID')}
              rows={data.members}
              truncated={data.members_truncated}
              error={data.members_error}
              testId="llm-proxy-usage-members"
            />
          </Box>

          {/* Stated once, at the bottom. A member breakdown that silently
              omitted service accounts would be read as a complete accounting of
              the spend above it, and the two would not sum. */}
          <Typography variant="bodySmall" color="text.secondary">
            {t(
              'pages.admin.llmProxy.usage.memberNote',
              'Requests made without a resolvable member — service accounts and token-authenticated integrations — are counted in the totals and in the project breakdown, but not under any member.',
            )}
          </Typography>
        </>
      ) : null}
    </Box>
  );
}
