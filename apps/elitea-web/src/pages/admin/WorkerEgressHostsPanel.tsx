/**
 * The hosts the platform's outbound HTTP reached and refused (ADR-0044
 * decision 2, #1490), shown under the Worker Egress section of the
 * Configuration page. An operator reads it before switching `mode` to
 * `allowlist`: "Copy as allowlist" puts one entry per reached host on the
 * clipboard, ready to paste into the section's allowlist field. Only the
 * worker's own hosts are listed: sandbox runs reach the egress proxy, which
 * has its own per-project allowlists, and copying them here would widen the
 * worker's list.
 */
import { useEffect, useState } from 'react';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';
import { handleCopy } from '@/shared/lib/clipboard';
import { t } from '@/shared/i18n';
import { allowlistFromReached, useWorkerEgressHosts } from './api/adminWorkerEgressApi';

const WINDOWS = [7, 30, 90] as const;

/** How long the button says "Copied" after a copy. */
const COPIED_MS = 2000;

function agentLabel(agentId: number | null): string {
  return agentId === null ? t('pages.admin.workerEgress.noAgent', 'No agent') : String(agentId);
}

type Hosts = ReturnType<typeof useWorkerEgressHosts>['data'];

export function WorkerEgressHostsPanel() {
  const [days, setDays] = useState<number>(30);
  const hosts = useWorkerEgressHosts(days);
  const truncated = hosts.data?.reached_truncated === true || hosts.data?.refused_truncated === true;

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }} data-testid="worker-egress-hosts">
      <Typography variant="headingSmall" component="h2">
        {t('pages.admin.workerEgress.title', 'Hosts reached')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.workerEgress.description',
          'Public hosts that agent tools reached from the worker, and the hosts the allowlist refused. Sandbox runs go through the egress proxy and are not listed. Review this before switching to allowlist mode, so the list holds what your agents already use.',
        )}
      </Typography>
      <Toolbar days={days} onDays={setDays} data={hosts.data} />
      {hosts.isError && (
        <Alert severity="error">{t('pages.admin.workerEgress.loadError', 'Could not read the reached hosts.')}</Alert>
      )}
      {truncated && (
        <Alert severity="info">
          {t('pages.admin.workerEgress.truncated', 'Only the most used hosts are shown. Narrow the time window to see the rest.')}
        </Alert>
      )}
      <ReachedTable data={hosts.data} loading={hosts.isLoading} />
      <RefusedTable data={hosts.data} />
    </Box>
  );
}

function Toolbar({ days, onDays, data }: { readonly days: number; readonly onDays: (days: number) => void; readonly data: Hosts }) {
  // "Copied" belongs to the list that was copied: another window, or a
  // refetch that changed the list, shows the button again, as does the timeout.
  const [copiedFrom, setCopiedFrom] = useState<Hosts>(undefined);
  const copied = copiedFrom !== undefined && copiedFrom === data;
  useEffect(() => {
    if (copiedFrom === undefined) return undefined;
    const timer = setTimeout(() => setCopiedFrom(undefined), COPIED_MS);
    return () => clearTimeout(timer);
  }, [copiedFrom]);
  const reached = data?.reached ?? [];
  return (
    <Box sx={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
      <Select
        size="small"
        value={days}
        onChange={(event) => onDays(Number(event.target.value))}
        inputProps={{ 'aria-label': t('pages.admin.workerEgress.window', 'Time window') }}
      >
        {WINDOWS.map((window) => (
          <MenuItem key={window} value={window}>
            {t('pages.admin.workerEgress.days', 'Last {{days}} days', { days: window })}
          </MenuItem>
        ))}
      </Select>
      <Button
        size="small"
        variant="secondary"
        disabled={reached.length === 0}
        onClick={() => {
          void handleCopy(allowlistFromReached(reached).join('\n')).then(() => setCopiedFrom(data));
        }}
        sx={{ textTransform: 'none' }}
      >
        {copied ? t('pages.admin.workerEgress.copied', 'Copied') : t('pages.admin.workerEgress.copy', 'Copy as allowlist')}
      </Button>
    </Box>
  );
}

function ReachedTable({ data, loading }: { readonly data: Hosts; readonly loading: boolean }) {
  const reached = data?.reached ?? [];
  return (
    <Table size="small" aria-label={t('pages.admin.workerEgress.reached', 'Reached hosts')} data-testid="worker-egress-reached">
      <TableHead>
        <TableRow>
          <TableCell>{t('pages.admin.workerEgress.column.host', 'Host')}</TableCell>
          <TableCell>{t('pages.admin.workerEgress.column.port', 'Port')}</TableCell>
          <TableCell>{t('pages.admin.workerEgress.column.project', 'Project')}</TableCell>
          <TableCell>{t('pages.admin.workerEgress.column.agent', 'Agent')}</TableCell>
          <TableCell>{t('pages.admin.workerEgress.column.runs', 'Runs')}</TableCell>
          <TableCell>{t('pages.admin.workerEgress.column.lastSeen', 'Last seen')}</TableCell>
        </TableRow>
      </TableHead>
      <TableBody>
        {reached.length === 0 && !loading && (
          <TableRow>
            <TableCell colSpan={6}>
              {t('pages.admin.workerEgress.emptyReached', 'No public host was reached in this window.')}
            </TableCell>
          </TableRow>
        )}
        {reached.map((row) => (
          <TableRow key={`${row.project_id}-${row.agent_id ?? 'none'}-${row.host}-${row.port}`}>
            <TableCell>{row.host}</TableCell>
            <TableCell>{row.port}</TableCell>
            <TableCell>{row.project_id}</TableCell>
            <TableCell>{agentLabel(row.agent_id)}</TableCell>
            <TableCell>{row.executions}</TableCell>
            <TableCell data-testid="worker-egress-time">{new Date(row.last_seen).toLocaleString()}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

function RefusedTable({ data }: { readonly data: Hosts }) {
  const refused = data?.refused ?? [];
  if (refused.length === 0) return null;
  return (
    <Table size="small" aria-label={t('pages.admin.workerEgress.refused', 'Refused hosts')} data-testid="worker-egress-refused">
      <TableHead>
        <TableRow>
          <TableCell>{t('pages.admin.workerEgress.column.refusedHost', 'Refused host')}</TableCell>
          <TableCell>{t('pages.admin.workerEgress.column.port', 'Port')}</TableCell>
          <TableCell>{t('pages.admin.workerEgress.column.reason', 'Reason')}</TableCell>
          <TableCell>{t('pages.admin.workerEgress.column.project', 'Project')}</TableCell>
          <TableCell>{t('pages.admin.workerEgress.column.agent', 'Agent')}</TableCell>
          <TableCell>{t('pages.admin.workerEgress.column.count', 'Refusals')}</TableCell>
          <TableCell>{t('pages.admin.workerEgress.column.lastSeen', 'Last seen')}</TableCell>
        </TableRow>
      </TableHead>
      <TableBody>
        {refused.map((row) => (
          <TableRow key={`${row.project_id}-${row.agent_id ?? 'none'}-${row.host}-${row.port}-${row.reason}`}>
            <TableCell>{row.host}</TableCell>
            <TableCell>{row.port}</TableCell>
            <TableCell>{row.reason}</TableCell>
            <TableCell>{row.project_id}</TableCell>
            <TableCell>{agentLabel(row.agent_id)}</TableCell>
            <TableCell>{row.count}</TableCell>
            <TableCell data-testid="worker-egress-time">{new Date(row.last_seen).toLocaleString()}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
