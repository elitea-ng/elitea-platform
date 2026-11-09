/**
 * The agent editor's Heartbeat tab (ADR-0050 decision 2, issue 1618): turn
 * the agent's heartbeat on or off, attach the owner's mandate, and read the
 * heartbeat history.
 *
 * The heartbeat block itself (interval, quiet hours, what it reads, limits)
 * is `meta.proactivity` of the version, edited with the agent and read from
 * the PUBLISHED release only; this tab shows the block the release has. The
 * server decides whether the caller may turn the heartbeat on
 * (`governance.can_enable` and `governance.reason`), so the tab never
 * derives it. Cloud agents only: the desktop app has no heartbeat in v1.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { AgentHeartbeatEntry, AgentHeartbeatSettings } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { refusalMessage } from '../api/agentHeartbeatApi';
import { useAgentHeartbeat, useAgentHeartbeatHistory, useSaveAgentHeartbeat } from '../model/useAgentHeartbeat';

export interface HeartbeatPanelProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
}

export function HeartbeatPanel({ projectId, applicationId }: HeartbeatPanelProps): ReactNode {
  if (projectId === undefined || projectId === '' || applicationId === undefined || applicationId <= 0) {
    return (
      <Typography variant="bodyMedium" color="text.secondary" data-testid="agent-heartbeat-unsaved">
        {t('features.agentHeartbeat.unsaved', 'Save and publish the agent first: the heartbeat runs the published release.')}
      </Typography>
    );
  }
  return <HeartbeatPanelBody projectId={projectId} applicationId={applicationId} />;
}

const REASON_TEXT: Readonly<Record<string, () => string>> = {
  proactivity_platform_off: () =>
    t('features.agentHeartbeat.reason.platformOff', 'Proactive agents are turned off on this platform.'),
  proactivity_tier_too_low: () =>
    t('features.agentHeartbeat.reason.tierTooLow', 'A proactive agent must be at least medium risk, with a reviewed release.'),
  proactivity_no_release: () => t('features.agentHeartbeat.reason.noRelease', 'The agent has no published release.'),
  proactivity_block_off: () =>
    t(
      'features.agentHeartbeat.reason.blockOff',
      'The published release has no proactivity block with mode heartbeat. Add one to the agent meta and publish.',
    ),
};

function reasonText(reason: string | undefined): string {
  return reason !== undefined ? (REASON_TEXT[reason]?.() ?? '') : '';
}

function HeartbeatPanelBody({ projectId, applicationId }: { readonly projectId: string; readonly applicationId: number }): ReactNode {
  const settings = useAgentHeartbeat(projectId, applicationId);
  if (settings.isPending) {
    return <CircularProgress size="1.5rem" data-testid="agent-heartbeat-loading" />;
  }
  if (settings.isError) {
    return (
      <Alert severity="error" data-testid="agent-heartbeat-error">
        {t('features.agentHeartbeat.loadError', 'The heartbeat settings could not be loaded.')}
      </Alert>
    );
  }
  return <HeartbeatSettingsView projectId={projectId} applicationId={applicationId} data={settings.data} />;
}

interface SettingsViewProps {
  readonly projectId: string;
  readonly applicationId: number;
  readonly data: AgentHeartbeatSettings;
}

function HeartbeatSettingsView({ projectId, applicationId, data }: SettingsViewProps): ReactNode {
  const history = useAgentHeartbeatHistory(projectId, applicationId);
  const save = useSaveAgentHeartbeat(projectId, applicationId);
  const refusal = save.isError
    ? (refusalMessage(save.error) ?? t('features.agentHeartbeat.saveError', 'The heartbeat could not be saved.'))
    : undefined;
  const blocked = !data.governance.can_enable && !data.enabled;

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }} data-testid="agent-heartbeat-panel">
      <Typography variant="bodyMedium" color="text.secondary">
        {t(
          'features.agentHeartbeat.intro',
          'The heartbeat wakes the agent on a timer. The agent reads its goals and the new messages, then does nothing, sends you a message, or starts a run. A run uses your mandate and waits for you at every approval.',
        )}
      </Typography>
      {blocked && (
        <Alert severity="info" data-testid="agent-heartbeat-reason">
          {reasonText(data.governance.reason)}
        </Alert>
      )}
      <ReleaseSummary settings={data} />
      <FormControlLabel
        control={
          <Switch
            checked={data.enabled}
            disabled={save.isPending || blocked}
            onChange={(_event, enabled) => save.mutate({ enabled })}
            slotProps={{ input: { 'aria-label': t('features.agentHeartbeat.toggle', 'Heartbeat on') } }}
            data-testid="agent-heartbeat-toggle"
          />
        }
        label={t('features.agentHeartbeat.toggle', 'Heartbeat on')}
      />
      <MandateRow data={data} save={save} />
      {refusal !== undefined && (
        <Alert severity="error" data-testid="agent-heartbeat-refusal">
          {refusal}
        </Alert>
      )}
      <Typography variant="headingSmall">{t('features.agentHeartbeat.history', 'Heartbeat history')}</Typography>
      <HistoryTable rows={history.data?.rows} loading={history.isPending} />
    </Box>
  );
}

function MandateRow({
  data,
  save,
}: {
  readonly data: AgentHeartbeatSettings;
  readonly save: ReturnType<typeof useSaveAgentHeartbeat>;
}): ReactNode {
  const [mandate, setMandate] = useState<string | undefined>(undefined);
  const value = mandate ?? data.mandate_id ?? '';
  return (
    <>
      <Box sx={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
        <TextField
          size="small"
          sx={{ minWidth: '24rem' }}
          label={t('features.agentHeartbeat.mandate', 'Mandate ID (a delegated mandate of yours)')}
          value={value}
          onChange={(event) => setMandate(event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'agent-heartbeat-mandate' } }}
        />
        <Button
          variant="outlined"
          disabled={save.isPending || mandate === undefined}
          onClick={() =>
            save.mutate({ enabled: data.enabled, mandate_id: value.trim() }, { onSuccess: () => setMandate(undefined) })
          }
          data-testid="agent-heartbeat-save-mandate"
        >
          {t('features.agentHeartbeat.saveMandate', 'Save mandate')}
        </Button>
      </Box>
      {data.enabled && data.mandate_id === null && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="agent-heartbeat-no-mandate">
          {t('features.agentHeartbeat.noMandate', 'Without a mandate the agent can send you messages but cannot start runs.')}
        </Typography>
      )}
    </>
  );
}

function ReleaseSummary({ settings }: { readonly settings: AgentHeartbeatSettings }): ReactNode {
  const { release, governance } = settings;
  if (release === null || release.mode !== 'heartbeat') {
    return null;
  }
  const parts = [
    t('features.agentHeartbeat.interval', 'Every {{interval}}', { interval: release.interval }),
    release.quiet_hours !== undefined && release.quiet_hours !== ''
      ? t('features.agentHeartbeat.quiet', 'quiet {{window}}', { window: release.quiet_hours })
      : undefined,
    t('features.agentHeartbeat.limits', 'at most {{wakes}} wakes, {{acts}} runs and {{spend}} USD a day', {
      wakes: String(governance.wakes_per_day),
      acts: String(governance.acts_per_day),
      spend: String(governance.spend_per_day_usd),
    }),
  ].filter((part): part is string => part !== undefined);
  return (
    <Typography variant="bodySmall" data-testid="agent-heartbeat-release">
      {parts.join(' · ')}
      {settings.last_skip_reason !== '' &&
        ` · ${t('features.agentHeartbeat.lastSkip', 'last skip: {{reason}}', { reason: settings.last_skip_reason })}`}
    </Typography>
  );
}

function HistoryTable({ rows, loading }: { readonly rows: readonly AgentHeartbeatEntry[] | undefined; readonly loading: boolean }): ReactNode {
  if (loading) {
    return <CircularProgress size="1.25rem" />;
  }
  if (rows === undefined || rows.length === 0) {
    return (
      <Typography variant="bodySmall" color="text.secondary" data-testid="agent-heartbeat-history-empty">
        {t('features.agentHeartbeat.historyEmpty', 'No wake yet.')}
      </Typography>
    );
  }
  return (
    <Table size="small" data-testid="agent-heartbeat-history">
      <TableHead>
        <TableRow>
          <TableCell>{t('features.agentHeartbeat.column.time', 'Time')}</TableCell>
          <TableCell>{t('features.agentHeartbeat.column.outcome', 'Outcome')}</TableCell>
          <TableCell>{t('features.agentHeartbeat.column.reason', 'Reason')}</TableCell>
        </TableRow>
      </TableHead>
      <TableBody>
        {rows.map((row) => (
          <TableRow key={row.id} data-testid="agent-heartbeat-row">
            <TableCell>{new Date(row.woke_at).toLocaleString()}</TableCell>
            <TableCell>{row.reason_code !== '' ? `${row.outcome} (${row.reason_code})` : row.outcome}</TableCell>
            <TableCell>{row.reason}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
