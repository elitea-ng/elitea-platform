/**
 * Admin › Configuration › LLM Proxy — **Logs**.
 *
 * ## What this shows that Usage cannot
 *
 * Usage reads the billing ledger, and a billing delta rides only a BILLED
 * request — so a call refused by a budget, rejected by a policy, addressed to an
 * unresolvable model or failed upstream is absent from it entirely. This reads
 * the per-request log, which records every request the gateway served whatever
 * happened to it. The failures ARE the reason this tab exists.
 *
 * ## The absence of payloads is stated, not left to be discovered
 *
 * There is no prompt, no completion and no upstream error text anywhere in this
 * table, and there cannot be: the schema has no column any of them could reach.
 * An operator who opens a log expecting to read a request needs to be told that
 * the answer is "reproduce it" rather than "look harder" — so the panel says so
 * once, plainly, instead of leaving a row that expands into nothing.
 */
import type { ReactNode } from 'react';
import { useDeferredValue, useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import FormControlLabel from '@mui/material/FormControlLabel';
import LinearProgress from '@mui/material/LinearProgress';
import Paper from '@mui/material/Paper';
import Switch from '@mui/material/Switch';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { UsageWindowSelect } from './LlmProxyModelsPanel';
import {
  useAdminLlmLogs,
  type LlmLogPage,
  type LlmLogSummary,
  type LlmRequestLogRow,
} from './api/adminLlmLogsApi';
import type { UsageWindow } from './api/adminLlmProxyApi';

/** Milliseconds, at a precision that stays readable across three orders of magnitude. */
function durationLabel(ms: number): string {
  if (ms < 1000) return `${String(ms)} ms`;
  return `${(ms / 1000).toFixed(2)} s`;
}

/** The row's total tokens, formatted. Zero is honest for a request that failed
 * before the provider answered. */
function tokenTotal(row: LlmRequestLogRow): string {
  return (row.prompt_tokens + row.completion_tokens).toLocaleString('en-US');
}

/** The status chip's register. 4xx is the caller's problem, 5xx is ours. */
function statusColour(status: number): 'success' | 'warning' | 'error' {
  if (status >= 500) return 'error';
  if (status >= 400) return 'warning';
  return 'success';
}

function LogSummaryRow({ summary }: { readonly summary: LlmLogSummary }): ReactNode {
  const tiles = [
    { label: t('pages.admin.llmLogs.requests', 'Requests'), value: summary.requests.toLocaleString('en-US') },
    { label: t('pages.admin.llmLogs.failed', 'Failed'), value: summary.failed.toLocaleString('en-US') },
    { label: t('pages.admin.llmLogs.median', 'Median'), value: durationLabel(summary.median_ms) },
    { label: t('pages.admin.llmLogs.p95', 'p95'), value: durationLabel(summary.p95_ms) },
  ];
  return (
    <Box sx={{ display: 'flex', gap: '0.75rem', flexWrap: 'wrap' }} data-testid="llm-logs-summary">
      {tiles.map((tile) => (
        <Paper
          key={tile.label}
          variant="outlined"
          sx={{ padding: '0.75rem 1rem', minWidth: '8rem', flex: '1 1 8rem' }}
        >
          <Typography variant="bodySmall" color="text.secondary">
            {tile.label}
          </Typography>
          <Typography variant="headingSmall" component="p">
            {tile.value}
          </Typography>
        </Paper>
      ))}
    </Box>
  );
}

function LogRows({ rows }: { readonly rows: readonly LlmRequestLogRow[] }): ReactNode {
  return (
    <TableContainer component={Paper} variant="outlined">
      <Table size="small" data-testid="llm-logs-table">
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.llmLogs.column.when', 'When (UTC)')}</TableCell>
            <TableCell>{t('pages.admin.llmLogs.column.status', 'Status')}</TableCell>
            <TableCell>{t('pages.admin.llmLogs.column.route', 'Route')}</TableCell>
            <TableCell>{t('pages.admin.llmLogs.column.model', 'Model')}</TableCell>
            <TableCell>{t('pages.admin.llmLogs.column.project', 'Project')}</TableCell>
            <TableCell align="right">{t('pages.admin.llmLogs.column.tokens', 'Tokens')}</TableCell>
            <TableCell align="right">{t('pages.admin.llmLogs.column.took', 'Took')}</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map((row) => (
            <TableRow key={row.id}>
              <TableCell>
                <Typography variant="bodySmall">{row.occurred_at.replace('T', ' ').replace('Z', '')}</Typography>
              </TableCell>
              <TableCell>
                <Chip
                  size="small"
                  variant="outlined"
                  color={statusColour(row.status)}
                  // The CODE beside the status, because "403" alone does not
                  // say whether a policy or a signature refused it.
                  label={row.error_code === '' ? String(row.status) : `${String(row.status)} ${row.error_code}`}
                />
              </TableCell>
              <TableCell>
                <Typography variant="bodySmall" color="text.secondary">
                  {row.route}
                  {row.streaming ? t('pages.admin.llmLogs.streamed', ' · streamed') : ''}
                </Typography>
              </TableCell>
              <TableCell>
                <Typography variant="bodySmall">
                  {row.model === '' ? '—' : row.model}
                </Typography>
              </TableCell>
              <TableCell>
                <Typography variant="bodySmall" color="text.secondary">
                  {row.project_id === null ? '—' : String(row.project_id)}
                </Typography>
              </TableCell>
              <TableCell align="right">
                <Typography variant="bodySmall">{tokenTotal(row)}</Typography>
              </TableCell>
              <TableCell align="right">
                <Typography variant="bodySmall">{durationLabel(row.duration_ms)}</Typography>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}

/**
 * The three ways the log read can be in trouble at once, each its own
 * statement.
 *
 * The transport failure and the server's own refusal are separate because they
 * call for different actions — a 403 on the route and "the log query timed out"
 * are not the same problem. The summary is separate again: the page is still
 * worth showing when only the window totals failed, and a zeroed summary
 * rendered without its reason reads as "no failures in this window".
 */
function LogAlerts({
  loadError,
  page,
}: {
  readonly loadError: unknown;
  readonly page: LlmLogPage | undefined;
}): ReactNode {
  return (
    <>
      {loadError != null ? (
        <Alert severity="warning" data-testid="llm-logs-load-error">
          {t('pages.admin.llmLogs.loadError', 'Failed to load the request log.')}
        </Alert>
      ) : null}
      {page?.error !== undefined ? (
        <Alert severity="warning" data-testid="llm-logs-error">
          {page.error}
        </Alert>
      ) : null}
      {page?.summary_error !== undefined ? (
        <Alert severity="warning" data-testid="llm-logs-summary-error">
          {page.summary_error}
        </Alert>
      ) : null}
    </>
  );
}

/**
 * The listing's three terminal states.
 *
 * The empty state is suppressed when the read FAILED — "no requests in this
 * window" is the reading that would tell an operator investigating an outage
 * that nothing was even being attempted. It also distinguishes the two empties:
 * with the failures filter on, "no failed requests" is good news, and the
 * general wording would bury it.
 */
function LogResults({
  isPending,
  failed,
  failedOnly,
  rows,
}: {
  readonly isPending: boolean;
  readonly failed: boolean;
  readonly failedOnly: boolean;
  readonly rows: readonly LlmRequestLogRow[];
}): ReactNode {
  if (isPending) {
    return <LinearProgress aria-label={t('pages.admin.llmLogs.loading', 'Loading the request log')} />;
  }
  if (rows.length > 0) return <LogRows rows={rows} />;
  if (failed) return null;
  return (
    <Typography variant="bodyMedium" color="text.secondary" data-testid="llm-logs-empty">
      {failedOnly
        ? t('pages.admin.llmLogs.noFailures', 'No failed requests in this window.')
        : t('pages.admin.llmLogs.empty', 'No requests in this window.')}
    </Typography>
  );
}

export function LlmProxyLogsPanel(): ReactNode {
  // `usageWindow`, not `window`: the latter shadows the DOM global for the
  // whole of this function — the naming rule `ModelsTab` states.
  const [usageWindow, setUsageWindow] = useState<UsageWindow>('24h');
  const [projectID, setProjectID] = useState('');
  const [model, setModel] = useState('');
  const [failedOnly, setFailedOnly] = useState(false);

  // The filters reach the SERVER, because the page is capped there: narrowing
  // only what was already returned would silently exclude every row past the
  // cap — the failure the cap itself has to avoid.
  const deferredProject = useDeferredValue(projectID);
  const deferredModel = useDeferredValue(model);

  const query = useAdminLlmLogs({
    window: usageWindow,
    projectID: deferredProject.trim(),
    model: deferredModel.trim(),
    failedOnly,
  });

  // Derived from `query.data?.pages`, which is stable between refetches,
  // rather than from a `?? []` fallback that is a fresh array every render.
  const rows = useMemo(
    () => (query.data?.pages ?? []).flatMap((page) => page.items),
    [query.data?.pages],
  );
  const first = query.data?.pages[0];

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.llmLogs.intro',
          'Every request the LLM gateway served — including the ones it refused, which the Usage report cannot show because only billed requests reach it.',
        )}
      </Typography>

      {/* Said once, plainly. An operator who opens a log expecting to read a
          request needs to know the answer is "reproduce it". */}
      <Typography variant="bodySmall" color="text.secondary" data-testid="llm-logs-no-payload">
        {t(
          'pages.admin.llmLogs.noPayload',
          'Prompts and responses are never recorded. This log stores what happened — the model, the outcome and the timing — and has no field that could hold the contents of a request.',
        )}
      </Typography>

      <Box sx={{ display: 'flex', gap: '1rem', alignItems: 'center', flexWrap: 'wrap' }}>
        <UsageWindowSelect usageWindow={usageWindow} onChange={setUsageWindow} />
        <TextField
          size="small"
          label={t('pages.admin.llmLogs.filter.project', 'Project ID')}
          value={projectID}
          onChange={(event) => setProjectID(event.target.value)}
          sx={{ width: '10rem' }}
          slotProps={{ htmlInput: { 'data-testid': 'llm-logs-project' } }}
        />
        <TextField
          size="small"
          label={t('pages.admin.llmLogs.filter.model', 'Model')}
          value={model}
          onChange={(event) => setModel(event.target.value)}
          sx={{ minWidth: '12rem' }}
          slotProps={{ htmlInput: { 'data-testid': 'llm-logs-model' } }}
        />
        <FormControlLabel
          control={
            <Switch
              checked={failedOnly}
              onChange={(event) => setFailedOnly(event.target.checked)}
              slotProps={{ input: { 'aria-label': t('pages.admin.llmLogs.filter.failed', 'Failures only') } }}
            />
          }
          label={t('pages.admin.llmLogs.filter.failed', 'Failures only')}
        />
      </Box>

      {/* The transport failure and the server's own refusal are separate
          statements: a 403 on the route and "the log query timed out" call for
          different actions. */}
      <LogAlerts loadError={query.error} page={first} />

      {first !== undefined ? <LogSummaryRow summary={first.summary} /> : null}

      <LogResults
        isPending={query.isPending}
        failed={query.error != null || first?.error !== undefined}
        failedOnly={failedOnly}
        rows={rows}
      />

      {query.hasNextPage ? (
        <Box>
          <Button
            size="small"
            variant="secondary"
            data-testid="llm-logs-more"
            disabled={query.isFetchingNextPage}
            onClick={() => void query.fetchNextPage()}
          >
            {query.isFetchingNextPage
              ? t('pages.admin.llmLogs.loadingMore', 'Loading…')
              : t('pages.admin.llmLogs.more', 'Load older requests')}
          </Button>
        </Box>
      ) : null}

      {/* How far back the log goes. Without it, an absent request reads as "it
          never happened" rather than "it is older than the log". */}
      {first !== undefined && first.retention_days > 0 ? (
        <Typography variant="bodySmall" color="text.secondary">
          {t('pages.admin.llmLogs.retention', 'Requests are kept for {{days}} days.', {
            days: first.retention_days,
          })}
        </Typography>
      ) : null}
    </Box>
  );
}
