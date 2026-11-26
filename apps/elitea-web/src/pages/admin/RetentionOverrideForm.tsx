/**
 * The project override form of Admin > Data retention, and the helpers the
 * policy rows share with it (issue 1495). Every write asks for the reason of
 * a dangerous action first (ADR-0059 R3.8b).
 */
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { AdminDangerousRequest } from './useAdminDangerousConfirm';
import { ADMIN_ACTION_PERMISSION, ADMIN_ITEM } from './adminItemIds';
import { useAdminActionControl, useAdminWritable } from './adminWriteAccess';
import { retentionFailureReason, useSetProjectOverride } from './api/adminRetentionApi';

export function classLabel(dataClass: string): string {
  switch (dataClass) {
    case 'conversation':
      return t('pages.admin.retention.class.conversation', 'Conversations');
    case 'attachment':
      return t('pages.admin.retention.class.attachment', 'Attachments');
    case 'step_content':
      return t('pages.admin.retention.class.stepContent', 'Run step content');
    case 'step_record':
      return t('pages.admin.retention.class.stepRecord', 'Run step records');
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

export /** Parses a day count field. Empty is null (keep forever); NaN is refused. */
function parseDays(value: string): number | null | 'invalid' {
  const trimmed = value.trim();
  if (trimmed === '') return null;
  if (!/^\d+$/.test(trimmed)) return 'invalid';
  return Number(trimmed);
}

export function errorMessage(error: unknown): string {
  return (
    retentionFailureReason(error) ?? t('pages.admin.retention.error.save', 'Failed to save the retention policy.')
  );
}

export /** The reach of a retention write, and the copy every write shares (ADR-0059 R3.8b). */
function retentionRequest(dataClass: string, projectId: number | undefined) {
  return {
    title: t('pages.admin.retention.confirm.title', 'Change the retention of {{name}}', { name: classLabel(dataClass) }),
    change: t(
      'pages.admin.retention.confirm.body',
      'A shorter maximum deletes the data older than it on the next sweep, and nothing restores it. A longer one keeps data that would have been deleted.',
    ),
    reach: { count: projectId === undefined ? undefined : 1, unit: 'projects' as const },
    target: dataClass,
    confirmLabel: t('pages.admin.retention.save', 'Save'),
  };
}

export function OverrideForm({
  classes,
  onError,
  onSaved,
  ask,
}: {
  readonly classes: readonly string[];
  readonly onError: (message: string) => void;
  readonly onSaved: () => void;
  readonly ask: (request: AdminDangerousRequest) => void;
}) {
  const access = useAdminWritable(ADMIN_ITEM.dataRetention);
  const locked = useAdminActionControl(ADMIN_ACTION_PERMISSION.retentionManage).disabled;
  const setOverride = access.guard(useSetProjectOverride());
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
    const draft = { dataClass, projectId: parsedProject, maxAgeDays: parsedMax };
    ask({
      ...retentionRequest(dataClass, parsedProject),
      action: 'PUT /api/v2/admin/retention/policies/{dataClass}/projects/{projectID} shorten',
      run: (reason) =>
        setOverride.mutate(
          { draft, reason },
          {
            onSuccess: () => {
              setProjectId('');
              setMaxDays('');
              onSaved();
            },
            onError: (error) => onError(errorMessage(error)),
          },
        ),
    });
  };

  return (
    <Box sx={{ display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
      <TextField
        select
        size="small"
        label={t('pages.admin.retention.override.class', 'Data class')}
        value={dataClass}
        disabled={locked}
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
        disabled={locked}
        onChange={(event) => setProjectId(event.target.value)}
      />
      <TextField
        size="small"
        label={t('pages.admin.retention.column.max', 'Maximum (days)')}
        placeholder={t('pages.admin.retention.forever', 'Forever')}
        value={maxDays}
        disabled={locked}
        onChange={(event) => setMaxDays(event.target.value)}
      />
      <Button variant="outlined" size="small" disabled={locked || setOverride.isPending} onClick={onSubmit}>
        {t('pages.admin.retention.override.set', 'Set override')}
      </Button>
    </Box>
  );
}
