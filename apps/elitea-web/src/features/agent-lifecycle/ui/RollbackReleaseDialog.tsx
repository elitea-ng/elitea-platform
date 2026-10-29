import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { ApplicationRelease } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useRollbackRelease } from '../api/releasesApi';
import { lifecycleErrorMessage } from '../model/useEntityLifecycle';

/**
 * Confirms a rollback to `target` and asks for the reason the audit event
 * keeps (the server takes at most 200 characters).
 */
export function RollbackReleaseDialog({
  projectId,
  applicationId,
  target,
  onClose,
}: {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly target: ApplicationRelease | undefined;
  readonly onClose: () => void;
}): ReactNode {
  const [reason, setReason] = useState('');
  const rollback = useRollbackRelease(projectId, applicationId);
  const close = (): void => {
    setReason('');
    rollback.reset();
    onClose();
  };
  return (
    <Dialog
      open={target !== undefined}
      onClose={close}
      data-testid="release-rollback-dialog"
    >
      <DialogTitle>
        {t('features.agentLifecycle.releases.rollbackTitle', 'Roll back to release #{{id}}?', { id: target?.id ?? '' })}
      </DialogTitle>
      <DialogContent>
        <Typography variant="bodyMedium">
          {t(
            'features.agentLifecycle.releases.rollbackBody',
            'New runs of this agent will use this release. Runs already in progress keep the release they started with.',
          )}
        </Typography>
        <TextField
          fullWidth
          margin="normal"
          label={t('features.agentLifecycle.releases.reason', 'Reason (recorded in the audit event)')}
          value={reason}
          onChange={(event) => setReason(event.target.value.slice(0, 200))}
        />
        {rollback.isError && <Alert severity="error">{lifecycleErrorMessage(rollback.error)}</Alert>}
      </DialogContent>
      <DialogActions>
        <Button onClick={close}>{t('features.agentLifecycle.releases.cancel', 'Cancel')}</Button>
        <Button
          variant="contained"
          disabled={target === undefined || rollback.isPending}
          data-testid="release-rollback-confirm"
          onClick={() => {
            if (target === undefined) return;
            rollback.mutate({ releaseId: target.id, reason: reason.trim() }, { onSuccess: close });
          }}
        >
          {t('features.agentLifecycle.releases.rollbackConfirm', 'Roll back')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

