/**
 * Admin › Configuration › LLM Proxy — the runtime status panel.
 *
 * ## The question this panel answers
 *
 * An operator authors a governance definition on `/admin/app/governance` and
 * then has to know whether the gateway is enforcing it. The governance table
 * cannot tell them: it says what was WRITTEN. This panel says what the gateway
 * HOLDS, and the difference between the two is the whole reason it exists —
 *
 *   - a row that was **rejected** at load and is enforcing nothing,
 *   - a row that parsed and is **inert**, matching nothing it could ever apply to,
 *   - a snapshot that is **stale** because refreshes are failing, so the rules in
 *     force are older than the ones on screen.
 *
 * Each of those looks identical from the authoring side, where the row is simply
 * present and enabled.
 *
 * ## Reachability leads, and nothing is shown as live without it
 *
 * Every number below describes a snapshot that may be minutes old or absent. If
 * the hop did not answer, showing those numbers at all would let a stale report
 * be read as a live one, so an unreachable gateway renders its reason and
 * nothing else.
 *
 * ## Two states are reported as problems even though nothing errored
 *
 * `rate_limits_enforceable: false` means an authored rate limit loads and does
 * nothing — the gateway has no NATS counter. The `degraded` count means requests
 * were admitted WITHOUT their ceiling applied. Neither produces an error
 * anywhere, and neither is visible from any other screen.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import LinearProgress from '@mui/material/LinearProgress';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { configFailureReason } from './api/adminConfigurationApi';
import {
  useGatewayStatus,
  type GatewayDiagnosticRow,
  type GatewayStatusBody,
} from './api/adminLlmProxyApi';

/** One labelled number in the summary strip. */
function StatTile({
  label,
  value,
  tone,
}: {
  readonly label: string;
  readonly value: string;
  readonly tone?: 'default' | 'warning' | 'error';
}) {
  const color =
    tone === 'error' ? 'error.main' : tone === 'warning' ? 'warning.main' : 'text.primary';
  return (
    <Box sx={{ minWidth: '8rem' }}>
      <Typography variant="bodySmall" color="text.secondary">
        {label}
      </Typography>
      <Typography variant="h6" sx={{ fontWeight: 600, color }}>
        {value}
      </Typography>
    </Box>
  );
}

/**
 * The table of rows the gateway could not use.
 *
 * Rejected and inert are rendered by the same component but never merged into
 * one list: a malformed row and a well-formed unenforceable row call for
 * different fixes, and the reason column is only meaningful next to which of the
 * two it is.
 */
function DiagnosticTable({
  rows,
  label,
  testId,
}: {
  readonly rows: readonly GatewayDiagnosticRow[];
  readonly label: string;
  readonly testId: string;
}) {
  if (rows.length === 0) return null;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.25rem' }}>
      <Typography variant="bodyMedium" sx={{ fontWeight: 600 }}>
        {label}
      </Typography>
      <TableContainer>
        <Table size="small" aria-label={label} data-testid={testId}>
          <TableHead>
            <TableRow>
              <TableCell>{t('pages.admin.llmProxy.status.column.name', 'Name')}</TableCell>
              <TableCell>{t('pages.admin.llmProxy.status.column.type', 'Type')}</TableCell>
              <TableCell>{t('pages.admin.llmProxy.status.column.reason', 'Reason')}</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {rows.map((row) => (
              <TableRow key={`${row.type}:${row.id}`}>
                <TableCell>{row.name}</TableCell>
                <TableCell>
                  <Chip size="small" label={row.type} />
                </TableCell>
                {/* The gateway's own sentence. These reasons are specific and
                    actionable ("CEL compile error: …"), and collapsing them into
                    a generic label would throw away the only words that say what
                    to change. */}
                <TableCell sx={{ color: 'text.secondary' }}>{row.reason}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </TableContainer>
    </Box>
  );
}

/**
 * The tone a count carries: a non-zero one is a problem, a zero one is not.
 *
 * Extracted because the same ternary appeared on every tile and the repetition
 * alone put the tile block over the complexity gate — and because "which counts
 * are bad news" is a rule worth stating once rather than five times.
 */
function countTone(
  count: number,
  whenNonZero: 'warning' | 'error',
): 'default' | 'warning' | 'error' {
  return count > 0 ? whenNonZero : 'default';
}

/**
 * The five counters.
 *
 * Split from the alerts below so each stays under the complexity gate, and
 * because the two answer different questions: the tiles say what the gateway
 * holds, the alerts say what is wrong with it.
 */
function SnapshotTiles({ gateway }: { readonly gateway: GatewayStatusBody }) {
  const definitions = gateway.definitions;
  const rejected = definitions?.rejected ?? [];
  const inert = definitions?.inert ?? [];
  const degraded = gateway.rate_limiter?.degraded ?? 0;

  return (
    <Box sx={{ display: 'flex', gap: '1.5rem', flexWrap: 'wrap' }}>
      <StatTile
        label={t('pages.admin.llmProxy.status.rows', 'Definitions loaded')}
        value={String(definitions?.rows ?? 0)}
      />
      <StatTile
        label={t('pages.admin.llmProxy.status.rejected', 'Rejected')}
        value={String(rejected.length)}
        tone={countTone(rejected.length, 'error')}
      />
      <StatTile
        label={t('pages.admin.llmProxy.status.inert', 'Inert')}
        value={String(inert.length)}
        tone={countTone(inert.length, 'warning')}
      />
      <StatTile
        label={t('pages.admin.llmProxy.status.refused', 'Rate-limited requests')}
        value={String(gateway.rate_limiter?.refused ?? 0)}
      />
      <StatTile
        label={t('pages.admin.llmProxy.status.degraded', 'Admitted without a ceiling')}
        value={String(degraded)}
        tone={countTone(degraded, 'error')}
      />
    </Box>
  );
}

/**
 * The three conditions that are wrong without anything having errored.
 *
 * A stale snapshot, rate limits that cannot be enforced, and requests already
 * admitted without their ceiling. None of them produces an error on any other
 * screen, which is why each is stated here rather than left to be inferred from
 * a counter.
 */
function SnapshotAlerts({ gateway }: { readonly gateway: GatewayStatusBody }) {
  const store = gateway.store;
  const degraded = gateway.rate_limiter?.degraded ?? 0;
  const stale = store?.error !== undefined && store.error !== '';

  return (
    <>
      {/* The state an operator is least likely to suspect: everything renders,
          the counts look right, and the rules in force are older than the ones
          the authoring page shows. */}
      {stale ? (
        <Alert severity="warning" data-testid="llm-proxy-status-stale">
          {t(
            'pages.admin.llmProxy.status.stale',
            'The gateway last refreshed its definitions successfully at {{at}}, and refreshes are now failing: {{error}}. The rules being enforced are the ones from that time, not the ones on the governance page.',
            { at: store?.last_success ?? '—', error: store?.error ?? '' },
          )}
        </Alert>
      ) : null}

      {gateway.rate_limits_enforceable === false ? (
        <Alert severity="warning" data-testid="llm-proxy-status-ratelimits">
          {t(
            'pages.admin.llmProxy.status.rateLimitsInert',
            'This gateway has no shared counter, so an authored rate limit loads and enforces nothing. Budgets are unaffected.',
          )}
        </Alert>
      ) : null}

      {degraded > 0 ? (
        <Alert severity="error" data-testid="llm-proxy-status-degraded">
          {t(
            'pages.admin.llmProxy.status.degradedDetail',
            '{{count}} request(s) were admitted without their authored rate limit applied, because the counter was unreachable.',
            { count: degraded },
          )}
        </Alert>
      ) : null}
    </>
  );
}

/**
 * The MERGED egress allowlist, tagged by source.
 *
 * This is the one control on the governance page whose effect an operator
 * cannot read off the row list. The gateway enforces the UNION of the authored
 * rows and the GATEWAY_EGRESS_ALLOWLIST the chart sets, so a row list alone
 * never answers "is this host permitted?" — and it never answers the question
 * that actually blocks a self-hosted endpoint, which is whether a private
 * address is reachable at all.
 */
function EgressSummary({ gateway }: { readonly gateway: GatewayStatusBody }) {
  const egress = gateway.egress;
  if (egress === undefined) return null;

  const env = egress.env ?? [];
  const db = egress.db ?? [];
  const effective = egress.effective ?? [];
  const dropped = egress.dropped ?? [];

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }} data-testid="llm-proxy-egress">
      <Typography variant="bodySmall" sx={{ fontWeight: 600 }}>
        {t('pages.admin.llmProxy.status.egressTitle', 'Egress allowlist in force')}
      </Typography>

      {egress.configured === true ? (
        <Typography variant="bodySmall" color="text.secondary" data-testid="llm-proxy-egress-effective">
          {t('pages.admin.llmProxy.status.egressEffective', 'Permitted destinations')}
          {': '}
          {effective.join(', ')}
        </Typography>
      ) : (
        <Alert severity="info" data-testid="llm-proxy-egress-unrestricted">
          {t(
            'pages.admin.llmProxy.status.egressUnrestricted',
            'No entry is configured anywhere, so a credential may name any public host. Private addresses stay unreachable. Adding the first entry turns the restriction on for every credential.',
          )}
        </Alert>
      )}

      <Typography variant="bodySmall" color="text.secondary" data-testid="llm-proxy-egress-sources">
        {t('pages.admin.llmProxy.status.egressFromChart', 'From the chart')}
        {`: ${env.length > 0 ? env.join(', ') : '—'} · `}
        {t('pages.admin.llmProxy.status.egressFromGovernance', 'From governance')}
        {`: ${db.length > 0 ? db.join(', ') : '—'}`}
      </Typography>

      {egress.private_network === true ? null : (
        <Alert severity="info" data-testid="llm-proxy-egress-private-closed">
          {t(
            'pages.admin.llmProxy.status.egressPrivateClosed',
            'No entry names a private address or block, so a self-hosted endpoint on a private network is not reachable. Add the host AND its block, for example 192.168.29.60:8000 and 192.168.29.0/24. A host name alone does not unlock it, because the gateway never resolves a name to decide.',
          )}
        </Alert>
      )}

      {dropped.length > 0 ? (
        <Alert severity="error" data-testid="llm-proxy-egress-dropped">
          {t(
            'pages.admin.llmProxy.status.egressDropped',
            'The gateway could not parse these authored entries, so they permit nothing: {{entries}}.',
            { entries: dropped.join(', ') },
          )}
        </Alert>
      ) : null}
    </Box>
  );
}

/** The counts and diagnostics of a gateway that answered. */
function LoadedSnapshot({ gateway }: { readonly gateway: GatewayStatusBody }) {
  const definitions = gateway.definitions;
  const store = gateway.store;

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      <SnapshotTiles gateway={gateway} />
      <SnapshotAlerts gateway={gateway} />

      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.llmProxy.status.freshness',
          'Snapshot loaded {{loadedAt}}. The gateway re-reads its definitions every {{interval}}, so a change saved on the governance page takes effect within that window.',
          {
            loadedAt: definitions?.loaded_at ?? '—',
            interval: store?.refresh_interval ?? '30s',
          },
        )}
      </Typography>

      <EgressSummary gateway={gateway} />

      <DiagnosticTable
        rows={definitions?.rejected ?? []}
        label={t(
          'pages.admin.llmProxy.status.rejectedTable',
          'Rejected — these did not load and are enforcing nothing',
        )}
        testId="llm-proxy-rejected-table"
      />
      <DiagnosticTable
        rows={definitions?.inert ?? []}
        label={t(
          'pages.admin.llmProxy.status.inertTable',
          'Inert — these loaded but can never match a request',
        )}
        testId="llm-proxy-inert-table"
      />
    </Box>
  );
}

export function LlmProxyStatusPanel() {
  const { data, isPending, error } = useGatewayStatus();

  if (isPending) {
    return (
      <LinearProgress aria-label={t('pages.admin.llmProxy.status.loading', 'Loading status')} />
    );
  }

  // A failure to reach THIS SERVICE, which is different from this service
  // reporting that it could not reach the gateway. The second is a result and
  // renders below; this one means the panel has no answer at all.
  if (error != null) {
    return (
      <Alert severity="warning" data-testid="llm-proxy-status-error">
        {configFailureReason(error) ??
          t('pages.admin.llmProxy.status.error', 'Failed to read the gateway status.')}
      </Alert>
    );
  }

  if (data === undefined || !data.reachable) {
    return (
      <Alert severity="warning" data-testid="llm-proxy-status-unreachable">
        {data?.error ??
          t('pages.admin.llmProxy.status.unreachable', 'The LLM gateway did not answer.')}
      </Alert>
    );
  }

  const gateway = data.gateway;

  // Reachable, and reporting that it reads no definitions at all — a gateway
  // with no database pool. Rendering zero counts here would say "nothing is
  // authored" when the truth is "nothing is being read".
  if (gateway === undefined || gateway.enabled === false) {
    return (
      <Alert severity="info" data-testid="llm-proxy-status-disabled">
        {t(
          'pages.admin.llmProxy.status.disabled',
          'The gateway answered but reads no governance definitions on this deployment, so nothing authored on the governance page is being enforced.',
        )}
      </Alert>
    );
  }

  return <LoadedSnapshot gateway={gateway} />;
}
