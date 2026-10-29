/**
 * The agent page's Stop agent action (ADR-0041 decision 1).
 *
 * Shown only to a caller with `safety.suspend` in the project (the project
 * admin): the button is presentation, the route is the authorisation. Stop
 * suspends this one agent: the server refuses its new runs with
 * `execution_suspended` and cancels its runs in flight. While the agent is
 * stopped the same place shows the state and a Resume action.
 */
import { useState, type ReactNode } from 'react';

import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

import {
  describeSuspensionError,
  useCanSuspend,
  useProjectSuspensions,
  useResumeProjectSuspension,
  useStopAgent,
} from '../api/suspensions';

export interface StopAgentButtonProps {
  readonly projectId: string | undefined;
  /** The agent's id, as the route carries it. */
  readonly applicationId: string | undefined;
  readonly onError: (message: string) => void;
}

export function StopAgentButton({ projectId, applicationId, onError }: StopAgentButtonProps): ReactNode {
  const canSuspend = useCanSuspend(projectId);
  const agentId = Number(applicationId);
  const valid = canSuspend && Number.isInteger(agentId) && agentId > 0;
  const suspensions = useProjectSuspensions(projectId, valid);
  const stop = useStopAgent(projectId);
  const resume = useResumeProjectSuspension(projectId);
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState('');

  if (!valid) return null;

  const active = (suspensions.data ?? []).find(
    (row) => row.scope === 'project' || (row.scope === 'agent' && row.application_id === agentId),
  );

  if (active !== undefined) {
    const projectWide = active.scope === 'project';
    return (
      <>
        <Tooltip title={active.reason}>
          <Chip
            color="error"
            size="small"
            data-testid="agent-stopped-chip"
            label={
              projectWide
                ? t('features.killSwitch.agent.projectStopped', 'Project stopped')
                : t('features.killSwitch.agent.stopped', 'Agent stopped')
            }
          />
        </Tooltip>
        {!projectWide && (
          <Button
            size="small"
            variant="outlined"
            disabled={resume.isPending}
            onClick={() =>
              resume.mutate(active.id, { onError: (error) => onError(describeSuspensionError(error)) })
            }
          >
            {t('features.killSwitch.agent.resume', 'Resume agent')}
          </Button>
        )}
      </>
    );
  }

  const close = (): void => {
    setOpen(false);
    setReason('');
  };

  return (
    <>
      <Button
        size="small"
        color="error"
        variant="outlined"
        data-testid="stop-agent-button"
        disabled={suspensions.isLoading}
        onClick={() => setOpen(true)}
      >
        {t('features.killSwitch.agent.stop', 'Stop agent')}
      </Button>
      <Dialog open={open} onClose={close} fullWidth maxWidth="sm">
        <DialogTitle>{t('features.killSwitch.agent.stopTitle', 'Stop this agent?')}</DialogTitle>
        <DialogContent>
          <DialogContentText>
            {t(
              'features.killSwitch.agent.stopDescription',
              'The agent cannot start new runs, and its runs in flight are cancelled. It stays stopped until an administrator resumes it.',
            )}
          </DialogContentText>
          <TextField
            fullWidth
            margin="normal"
            label={t('features.killSwitch.reason', 'Reason')}
            value={reason}
            slotProps={{ htmlInput: { maxLength: 1024, 'data-testid': 'stop-agent-reason' } }}
            onChange={(event) => setReason(event.target.value)}
          />
        </DialogContent>
        <DialogActions>
          <Button onClick={close}>{t('common.cancel', 'Cancel')}</Button>
          <Button
            color="error"
            variant="contained"
            data-testid="stop-agent-confirm"
            disabled={reason.trim() === '' || stop.isPending}
            onClick={() =>
              stop.mutate(
                { applicationId: agentId, reason: reason.trim() },
                { onSuccess: close, onError: (error) => onError(describeSuspensionError(error)) },
              )
            }
          >
            {t('features.killSwitch.agent.stopConfirm', 'Stop agent')}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}
