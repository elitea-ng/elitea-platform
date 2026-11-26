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

import { OverrideForm, classLabel, errorMessage, parseDays, retentionRequest } from './RetentionOverrideForm';
import { useAdminDangerousConfirm, type AdminDangerousRequest } from './useAdminDangerousConfirm';
import { ADMIN_ACTION_PERMISSION, ADMIN_ITEM } from './adminItemIds';
import { useAdminActionControl, useAdminWritable } from './adminWriteAccess';
import { useDeleteProjectOverride, useSetPlatformPolicy } from './api/adminRetentionApi';

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

function PlatformRow({
  policy,
  onError,
  onSaved,
  ask,
}: {
  readonly policy: RetentionPolicy;
  readonly onError: (message: string) => void;
  readonly onSaved: () => void;
  readonly ask: (request: AdminDangerousRequest) => void;
}) {
  const access = useAdminWritable(ADMIN_ITEM.dataRetention);
  const locked = useAdminActionControl(ADMIN_ACTION_PERMISSION.retentionManage).disabled;
  const save = access.guard(useSetPlatformPolicy());
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
    const draft = {
      dataClass: policy.data_class,
      minAgeDays: parsedMin ?? 0,
      maxAgeDays: parsedMax,
      allowProjectOverride: allowOverride,
    };
    ask({
      ...retentionRequest(policy.data_class, undefined),
      action: 'PUT /api/v2/admin/retention/policies/{dataClass} shorten',
      run: (reason) =>
        save.mutate({ draft, reason }, { onSuccess: onSaved, onError: (error) => onError(errorMessage(error)) }),
    });
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
          disabled={locked}
          onChange={(event) => setMinDays(event.target.value)}
          slotProps={{ htmlInput: { 'aria-label': `${t('pages.admin.retention.column.min', 'Minimum (days)')}: ${label}` } }}
          sx={{ width: '7rem' }}
        />
      </TableCell>
      <TableCell>
        <TextField
          size="small"
          value={maxDays}
          disabled={locked}
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
            disabled={locked}
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
        <Button variant="outlined" size="small" disabled={locked || save.isPending} onClick={onSave}>
          {t('pages.admin.retention.save', 'Save')}
        </Button>
      </TableCell>
    </TableRow>
  );
}

export function RetentionPolicies({ policies }: { readonly policies: readonly RetentionPolicy[] }) {
  const access = useAdminWritable(ADMIN_ITEM.dataRetention);
  const locked = useAdminActionControl(ADMIN_ACTION_PERMISSION.retentionManage).disabled;
  const removeOverride = access.guard(useDeleteProjectOverride());
  const confirm = useAdminDangerousConfirm();
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
              ask={confirm.ask}
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
                    disabled={locked || removeOverride.isPending}
                    onClick={() =>
                      confirm.ask({
                        ...retentionRequest(policy.data_class, policy.project_id ?? 0),
                        action: 'DELETE /api/v2/admin/retention/policies/{dataClass}/projects/{projectID} shorten',
                        run: (reason) =>
                          removeOverride.mutate(
                            { dataClass: policy.data_class, projectId: policy.project_id ?? 0, reason },
                            { onSuccess: onSaved, onError: (failure) => onError(errorMessage(failure)) },
                          ),
                      })
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
      <OverrideForm classes={overridable} onError={onError} onSaved={onSaved} ask={confirm.ask} />
      {confirm.dialog}
    </Box>
  );
}
