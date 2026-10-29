/**
 * The Retention policy section of Admin > Data retention (issue 1495).
 *
 * One editable row per data class, and the project overrides below. An empty
 * maximum keeps the data forever. The server validates every bound; its
 * refusal text is shown as it is.
 */
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import MenuItem from '@mui/material/MenuItem';
import Switch from '@mui/material/Switch';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { RetentionPolicy } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import {
  retentionFailureReason,
  useDeleteProjectOverride,
  useSetPlatformPolicy,
  useSetProjectOverride,
} from './api/adminRetentionApi';

function classLabel(dataClass: string): string {
  switch (dataClass) {
    case 'conversation':
      return t('pages.admin.retention.class.conversation', 'Conversations');
    case 'attachment':
      return t('pages.admin.retention.class.attachment', 'Attachments');
    case 'gateway_request_log':
      return t('pages.admin.retention.class.gatewayRequestLog', 'LLM gateway request log');
    case 'admin_audit':
      return t('pages.admin.retention.class.adminAudit', 'Admin audit trail');
    case 'security_audit':
      return t('pages.admin.retention.class.securityAudit', 'Security audit outbox');
    default:
      return dataClass;
  }
}

function classNote(policy: RetentionPolicy): string | undefined {
  switch (policy.data_class) {
    case 'gateway_request_log':
      return t(
        'pages.admin.retention.note.gateway',
        'The LLM gateway reads this value on its hourly prune. It cannot keep the log forever.',
      );
    case 'admin_audit':
    case 'security_audit':
      return policy.updated_by === null
        ? t(
            'pages.admin.retention.note.deploymentEnv',
            'Not saved yet: the deployment setting applies until you save this row.',
          )
        : undefined;
    case 'attachment':
      return t(
        'pages.admin.retention.note.attachment',
        'Attachments are deleted with their conversation. A separate maximum is not available yet.',
      );
    default:
      return undefined;
  }
}

/** Parses a day count field. Empty is null (keep forever); NaN is refused. */
function parseDays(value: string): number | null | 'invalid' {
  const trimmed = value.trim();
  if (trimmed === '') return null;
  if (!/^\d+$/.test(trimmed)) return 'invalid';
  return Number(trimmed);
}

function errorMessage(error: unknown): string {
  return (
    retentionFailureReason(error) ?? t('pages.admin.retention.error.save', 'Failed to save the retention policy.')
  );
}

function PlatformRow({
  policy,
  onError,
  onSaved,
}: {
  readonly policy: RetentionPolicy;
  readonly onError: (message: string) => void;
  readonly onSaved: () => void;
}) {
  const save = useSetPlatformPolicy();
  const [minDays, setMinDays] = useState(String(policy.min_age_days));
  const [maxDays, setMaxDays] = useState(policy.max_age_days === null ? '' : String(policy.max_age_days));
  const [allowOverride, setAllowOverride] = useState(policy.allow_project_override);
  const note = classNote(policy);
  const label = classLabel(policy.data_class);

  const onSave = () => {
    const parsedMin = parseDays(minDays);
    const parsedMax = parseDays(maxDays);
    if (parsedMin === 'invalid' || parsedMax === 'invalid') {
      onError(t('pages.admin.retention.error.days', 'Enter a whole number of days.'));
      return;
    }
    save.mutate(
      {
        dataClass: policy.data_class,
        minAgeDays: parsedMin ?? 0,
        maxAgeDays: parsedMax,
        allowProjectOverride: allowOverride,
      },
      { onSuccess: onSaved, onError: (error) => onError(errorMessage(error)) },
    );
  };

  return (
    <TableRow data-testid={`admin-retention-row-${policy.data_class}`}>
      <TableCell>
        <Typography variant="bodyMedium">{label}</Typography>
        <Typography variant="bodySmall" color="text.secondary" component="div">
          {policy.enforced_by}
        </Typography>
        {note ? (
          <Typography variant="bodySmall" color="text.secondary" component="div">
            {note}
          </Typography>
        ) : null}
      </TableCell>
      <TableCell>
        <TextField
          size="small"
          value={minDays}
          onChange={(event) => setMinDays(event.target.value)}
          slotProps={{ htmlInput: { 'aria-label': `${t('pages.admin.retention.column.min', 'Minimum (days)')}: ${label}` } }}
          sx={{ width: '7rem' }}
        />
      </TableCell>
      <TableCell>
        <TextField
          size="small"
          value={maxDays}
          placeholder={t('pages.admin.retention.forever', 'Forever')}
          onChange={(event) => setMaxDays(event.target.value)}
          slotProps={{ htmlInput: { 'aria-label': `${t('pages.admin.retention.column.max', 'Maximum (days)')}: ${label}` } }}
          sx={{ width: '7rem' }}
        />
        <Typography variant="bodySmall" color="text.secondary" component="div">
          {t('pages.admin.retention.floor', 'Floor')}: {policy.floor_days}
        </Typography>
      </TableCell>
      <TableCell>
        {policy.overrides_supported ? (
          <Switch
            checked={allowOverride}
            onChange={(event) => setAllowOverride(event.target.checked)}
            slotProps={{ input: { 'aria-label': `${t('pages.admin.retention.column.override', 'Project overrides')}: ${label}` } }}
          />
        ) : (
          <Typography variant="bodySmall" color="text.secondary">
            {t('pages.admin.retention.overrideUnsupported', 'Not available')}
          </Typography>
        )}
      </TableCell>
      <TableCell>
        <Button variant="outlined" size="small" disabled={save.isPending} onClick={onSave}>
          {t('pages.admin.retention.save', 'Save')}
        </Button>
      </TableCell>
    </TableRow>
  );
}

function OverrideForm({
  classes,
  onError,
  onSaved,
}: {
  readonly classes: readonly string[];
  readonly onError: (message: string) => void;
  readonly onSaved: () => void;
}) {
  const setOverride = useSetProjectOverride();
  const [dataClass, setDataClass] = useState(classes[0] ?? '');
  const [projectId, setProjectId] = useState('');
  const [maxDays, setMaxDays] = useState('');

  if (classes.length === 0) {
    return (
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.retention.override.none',
          'No data class allows project overrides. Switch on "Project overrides" for a class to allow them.',
        )}
      </Typography>
    );
  }

  const onSubmit = () => {
    const parsedProject = parseDays(projectId);
    const parsedMax = parseDays(maxDays);
    if (parsedProject === null || parsedProject === 'invalid' || parsedProject < 1 || parsedMax === 'invalid') {
      onError(t('pages.admin.retention.error.override', 'Enter a project id and a whole number of days.'));
      return;
    }
    setOverride.mutate(
      { dataClass, projectId: parsedProject, maxAgeDays: parsedMax },
      {
        onSuccess: () => {
          setProjectId('');
          setMaxDays('');
          onSaved();
        },
        onError: (error) => onError(errorMessage(error)),
      },
    );
  };

  return (
    <Box sx={{ display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
      <TextField
        select
        size="small"
        label={t('pages.admin.retention.override.class', 'Data class')}
        value={dataClass}
        onChange={(event) => setDataClass(event.target.value)}
        sx={{ minWidth: '12rem' }}
      >
        {classes.map((item) => (
          <MenuItem key={item} value={item}>
            {classLabel(item)}
          </MenuItem>
        ))}
      </TextField>
      <TextField
        size="small"
        label={t('pages.admin.retention.override.project', 'Project id')}
        value={projectId}
        onChange={(event) => setProjectId(event.target.value)}
      />
      <TextField
        size="small"
        label={t('pages.admin.retention.column.max', 'Maximum (days)')}
        placeholder={t('pages.admin.retention.forever', 'Forever')}
        value={maxDays}
        onChange={(event) => setMaxDays(event.target.value)}
      />
      <Button variant="outlined" size="small" disabled={setOverride.isPending} onClick={onSubmit}>
        {t('pages.admin.retention.override.set', 'Set override')}
      </Button>
    </Box>
  );
}

export function RetentionPolicies({ policies }: { readonly policies: readonly RetentionPolicy[] }) {
  const removeOverride = useDeleteProjectOverride();
  const [error, setError] = useState('');
  const [saved, setSaved] = useState(false);

  const platform = policies.filter((policy) => policy.project_id === null);
  const overrides = policies.filter((policy) => policy.project_id !== null);
  const overridable = platform
    .filter((policy) => policy.overrides_supported && policy.allow_project_override)
    .map((policy) => policy.data_class);

  const onError = (message: string) => {
    setSaved(false);
    setError(message);
  };
  const onSaved = () => {
    setError('');
    setSaved(true);
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      {error !== '' ? (
        <Alert severity="error" onClose={() => setError('')} data-testid="admin-retention-error">
          {error}
        </Alert>
      ) : null}
      {saved ? (
        <Alert severity="success" onClose={() => setSaved(false)} data-testid="admin-retention-saved">
          {t('pages.admin.retention.saved', 'Retention policy saved. It applies on the next sweep.')}
        </Alert>
      ) : null}

      <Table size="small" aria-label={t('pages.admin.retention.tab.policies', 'Retention policy')}>
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.retention.column.class', 'Data class')}</TableCell>
            <TableCell>{t('pages.admin.retention.column.min', 'Minimum (days)')}</TableCell>
            <TableCell>{t('pages.admin.retention.column.max', 'Maximum (days)')}</TableCell>
            <TableCell>{t('pages.admin.retention.column.override', 'Project overrides')}</TableCell>
            <TableCell />
          </TableRow>
        </TableHead>
        <TableBody>
          {platform.map((policy) => (
            <PlatformRow
              key={`${policy.data_class}:${policy.updated_at}`}
              policy={policy}
              onError={onError}
              onSaved={onSaved}
            />
          ))}
        </TableBody>
      </Table>

      <Typography variant="headingSmall" component="h2">
        {t('pages.admin.retention.override.title', 'Project overrides')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.retention.override.description',
          'A project can keep data for a different time, inside the platform minimum and maximum.',
        )}
      </Typography>
      {overrides.length > 0 ? (
        <Table size="small" aria-label={t('pages.admin.retention.override.title', 'Project overrides')}>
          <TableHead>
            <TableRow>
              <TableCell>{t('pages.admin.retention.column.class', 'Data class')}</TableCell>
              <TableCell>{t('pages.admin.retention.override.project', 'Project id')}</TableCell>
              <TableCell>{t('pages.admin.retention.column.max', 'Maximum (days)')}</TableCell>
              <TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {overrides.map((policy) => (
              <TableRow key={`${policy.data_class}:${policy.project_id}`}>
                <TableCell>{classLabel(policy.data_class)}</TableCell>
                <TableCell>{policy.project_id}</TableCell>
                <TableCell>
                  {policy.max_age_days ?? t('pages.admin.retention.forever', 'Forever')}
                </TableCell>
                <TableCell>
                  <Button
                    size="small"
                    disabled={removeOverride.isPending}
                    onClick={() =>
                      removeOverride.mutate(
                        { dataClass: policy.data_class, projectId: policy.project_id ?? 0 },
                        { onSuccess: onSaved, onError: (failure) => onError(errorMessage(failure)) },
                      )
                    }
                  >
                    {t('pages.admin.retention.override.remove', 'Remove')}
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : null}
      <OverrideForm classes={overridable} onError={onError} onSaved={onSaved} />
    </Box>
  );
}
