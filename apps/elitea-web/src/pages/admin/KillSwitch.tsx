/**
 * Admin › Kill switch (ADR-0041 decision 1): the active suspensions, Resume,
 * and Suspend for every scope, the platform-wide stop included.
 *
 * A suspension makes admission refuse the runs it covers with
 * `execution_suspended` and cancels the runs in flight; see the embedded
 * docs page Runtime safety, section Suspension and kill switch. The list
 * refreshes every ten seconds, so a stop made by another administrator or by
 * the scheduled self-test shows here without a reload.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import LinearProgress from '@mui/material/LinearProgress';
import Switch from '@mui/material/Switch';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { describeSuspensionError, type ExecutionSuspensionRow } from '@/features/kill-switch';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminSuspendDialog, resumeRuleLabel, scopeLabel } from './AdminSuspendDialog';
import { useAdminSuspensions, useCreateAdminSuspension, useResumeAdminSuspension } from './api/adminKillSwitchApi';

/** What the suspension covers, in one cell. */
export function suspensionTarget(row: ExecutionSuspensionRow): string {
  switch (row.scope) {
    case 'platform':
      return t('pages.admin.killSwitch.target.platform', 'Every run on the platform');
    case 'project':
      return `${t('pages.admin.killSwitch.target.project', 'Project')} ${row.project_id ?? ''}`;
    case 'agent':
      return `${t('pages.admin.killSwitch.target.project', 'Project')} ${row.project_id ?? ''}, ${t('pages.admin.killSwitch.target.agent', 'agent')} ${row.application_id ?? ''}`;
    case 'toolkit_type':
      return row.toolkit_type ?? '';
    default:
      return row.scope;
  }
}

export interface AdminKillSwitchProps {
  /** The auto-refresh interval in milliseconds, or false for none (tests). */
  readonly refetchInterval?: number | false;
}

export function AdminKillSwitch(props: AdminKillSwitchProps = {}): ReactNode {
  const [showHistory, setShowHistory] = useState(false);
  const [dialogKey, setDialogKey] = useState(0);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [error, setError] = useState<string>();
  const list = useAdminSuspensions(!showHistory, props.refetchInterval ?? 10_000);
  const create = useCreateAdminSuspension();
  const resume = useResumeAdminSuspension();
  const rows = list.data ?? [];

  const closeDialog = (): void => {
    setDialogOpen(false);
    setDialogKey((key) => key + 1);
    create.reset();
  };

  return (
    <DrawerPage>
      <Typography variant="headingLarge" component="h1" sx={titleSx}>
        {t('pages.admin.killSwitch.title', 'Kill switch')}
      </Typography>
      <Typography variant="bodyMedium" color="text.secondary" sx={subtitleSx}>
        {t(
          'pages.admin.killSwitch.subtitle',
          'Stop an agent, a project, a toolkit type or the whole platform. New runs are refused and runs in flight are cancelled until a suspension is resumed.',
        )}
      </Typography>

      <Box sx={toolbarSx}>
        <Button color="error" variant="contained" data-testid="kill-switch-open" onClick={() => setDialogOpen(true)}>
          {t('pages.admin.killSwitch.suspend', 'Suspend')}
        </Button>
        <FormControlLabel
          control={<Switch checked={showHistory} onChange={(event) => setShowHistory(event.target.checked)} />}
          label={t('pages.admin.killSwitch.showHistory', 'Show resumed suspensions')}
        />
      </Box>

      {list.isFetching && <LinearProgress data-testid="kill-switch-loading" />}
      {list.isError && (
        <Alert severity="error">{t('pages.admin.killSwitch.loadError', 'Could not read the suspensions.')}</Alert>
      )}
      {error !== undefined && (
        <Alert severity="error" onClose={() => setError(undefined)}>{error}</Alert>
      )}

      <Table size="small" data-testid="kill-switch-table">
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.killSwitch.column.scope', 'Scope')}</TableCell>
            <TableCell>{t('pages.admin.killSwitch.column.target', 'Target')}</TableCell>
            <TableCell>{t('pages.admin.killSwitch.column.reason', 'Reason')}</TableCell>
            <TableCell>{t('pages.admin.killSwitch.column.created', 'Suspended')}</TableCell>
            <TableCell>{t('pages.admin.killSwitch.column.cancelled', 'Runs cancelled')}</TableCell>
            <TableCell>{t('pages.admin.killSwitch.column.resume', 'Who can resume')}</TableCell>
            <TableCell />
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.length === 0 && !list.isLoading && (
            <TableRow>
              <TableCell colSpan={7}>
                {t('pages.admin.killSwitch.empty', 'No suspension is active. Every run is admitted.')}
              </TableCell>
            </TableRow>
          )}
          {rows.map((row) => (
            <TableRow key={row.id} data-testid={`kill-switch-row-${row.id}`}>
              <TableCell>{scopeLabel(row.scope)}</TableCell>
              <TableCell>{suspensionTarget(row)}</TableCell>
              <TableCell>{row.reason}</TableCell>
              <TableCell>{new Date(row.created_at).toLocaleString()}</TableCell>
              <TableCell>{row.cancelled_runs}</TableCell>
              <TableCell>{resumeRuleLabel(row.resume_requires)}</TableCell>
              <TableCell align="right">
                {row.active ? (
                  <Button
                    size="small"
                    disabled={resume.isPending}
                    onClick={() =>
                      resume.mutate(
                        { id: row.id, reason: '' },
                        { onError: (failure) => setError(describeSuspensionError(failure)) },
                      )
                    }
                  >
                    {t('pages.admin.killSwitch.resume', 'Resume')}
                  </Button>
                ) : (
                  <Typography variant="bodySmall" color="text.secondary">
                    {t('pages.admin.killSwitch.resumed', 'Resumed')}
                  </Typography>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>

      <AdminSuspendDialog
        key={dialogKey}
        open={dialogOpen}
        pending={create.isPending}
        error={create.isError ? describeSuspensionError(create.error) : undefined}
        onClose={closeDialog}
        onSubmit={(body) => create.mutate(body, { onSuccess: closeDialog })}
      />
    </DrawerPage>
  );
}

const titleSx: SxProps<Theme> = (theme) => ({ marginBottom: theme.spacing(0.5) });
const subtitleSx: SxProps<Theme> = (theme) => ({ marginBottom: theme.spacing(2) });
const toolbarSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(2),
  marginBottom: theme.spacing(2),
});
