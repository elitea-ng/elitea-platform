/**
 * The per-project exceptions for one toolkit type.
 *
 * An exception is the "this client may use `sql` and nobody else may" case, and
 * its mirror: "everyone but this client". It hangs off a deployment decision —
 * the server answers 409 for a grant with no decision — so this dialog is only
 * reachable from a row that already has one.
 *
 * ## The list is shown before the form
 *
 * An operator opening this asks "who has it" far more often than "give it to
 * someone". The existing exceptions, with their reasons and who filed them, are
 * therefore the first thing on screen and the add form is below them.
 */
import { useCallback, useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';
import MenuItem from '@mui/material/MenuItem';
import Stack from '@mui/material/Stack';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import {
  toolkitTypeFailureReason,
  useRevokeToolkitTypeGrant,
  useSaveToolkitTypeGrant,
  type AdminToolkitType,
  type ToolkitTypeGrantAvailability,
} from './api/adminToolkitTypesApi';

export interface ToolkitTypeProjectsDialogProps {
  /** `null` closes it. */
  readonly toolkitType: AdminToolkitType | null;
  readonly onClose: () => void;
}

export function ToolkitTypeProjectsDialog({ toolkitType, onClose }: ToolkitTypeProjectsDialogProps) {
  const saveGrant = useSaveToolkitTypeGrant();
  const revokeGrant = useRevokeToolkitTypeGrant();

  const [projectId, setProjectId] = useState('');
  const [availability, setAvailability] = useState<ToolkitTypeGrantAvailability>('enabled');
  const [reason, setReason] = useState('');
  const [failure, setFailure] = useState<string | null>(null);

  const numericProjectId = Number.parseInt(projectId.trim(), 10);
  const trimmedReason = reason.trim();
  const submitting = saveGrant.isPending || revokeGrant.isPending;
  const submittable =
    Number.isInteger(numericProjectId) && numericProjectId > 0 && trimmedReason !== '' && !submitting;

  const close = useCallback(() => {
    setProjectId('');
    setReason('');
    setFailure(null);
    onClose();
  }, [onClose]);

  const reportFailure = useCallback((error: unknown) => {
    setFailure(
      toolkitTypeFailureReason(error) ??
        t('pages.admin.toolkitTypes.error.grant', 'The project exception could not be changed.'),
    );
  }, []);

  const submit = useCallback(async () => {
    if (toolkitType === null || !submittable) return;
    setFailure(null);
    try {
      await saveGrant.mutateAsync({
        type: toolkitType.type,
        projectId: numericProjectId,
        availability,
        reason: trimmedReason,
      });
      setProjectId('');
      setReason('');
    } catch (error) {
      reportFailure(error);
    }
  }, [
    availability,
    numericProjectId,
    reportFailure,
    saveGrant,
    submittable,
    toolkitType,
    trimmedReason,
  ]);

  const revoke = useCallback(
    async (grantProjectId: number) => {
      if (toolkitType === null) return;
      setFailure(null);
      try {
        await revokeGrant.mutateAsync({ type: toolkitType.type, projectId: grantProjectId });
      } catch (error) {
        reportFailure(error);
      }
    },
    [reportFailure, revokeGrant, toolkitType],
  );

  return (
    <Dialog open={toolkitType !== null} onClose={close} fullWidth maxWidth="sm">
      <DialogTitle>
        {t('pages.admin.toolkitTypes.projects.title', 'Project exceptions')}
      </DialogTitle>
      <DialogContent>
        <DialogContentText sx={{ marginBottom: '1rem' }}>
          {t(
            'pages.admin.toolkitTypes.projects.body',
            'An exception overrides the deployment decision for one project. A disabled decision cannot be overridden.',
          )}
        </DialogContentText>
        {toolkitType === null ? null : (
          <Typography variant="body2" color="text.secondary" sx={{ marginBottom: '1rem' }}>
            {toolkitType.label} · {toolkitType.type}
          </Typography>
        )}

        <div data-testid="admin-toolkit-types-grants">
          {toolkitType === null || toolkitType.project_grants.length === 0 ? (
            <Typography variant="body2" color="text.secondary">
              {t('pages.admin.toolkitTypes.projects.none', 'No project exception is recorded.')}
            </Typography>
          ) : (
            toolkitType.project_grants.map((grant) => (
              <Stack
                key={grant.project_id}
                direction="row"
                sx={{ alignItems: 'center', justifyContent: 'space-between', marginBottom: '0.5rem' }}
              >
                <Typography variant="body2">
                  {t('pages.admin.toolkitTypes.projects.row', 'Project {{project}} — {{availability}}', {
                    project: grant.project_id,
                    availability: grant.availability,
                  })}
                  <Typography variant="caption" color="text.secondary" component="div">
                    {grant.reason} · {grant.granted_by}
                  </Typography>
                </Typography>
                <Button
                  size="small"
                  color="warning"
                  disabled={submitting}
                  onClick={() => {
                    void revoke(grant.project_id);
                  }}
                >
                  {t('pages.admin.toolkitTypes.action.revoke', 'Revoke')}
                </Button>
              </Stack>
            ))
          )}
        </div>

        <Stack direction="row" spacing={1} sx={{ marginTop: '1rem' }}>
          <TextField
            required
            label={t('pages.admin.toolkitTypes.projects.projectId', 'Project ID')}
            value={projectId}
            onChange={(event) => setProjectId(event.target.value)}
            sx={{ width: '10rem' }}
          />
          <TextField
            select
            label={t('pages.admin.toolkitTypes.projects.availability', 'Exception')}
            value={availability}
            onChange={(event) => setAvailability(event.target.value as ToolkitTypeGrantAvailability)}
            sx={{ width: '10rem' }}
          >
            <MenuItem value="enabled">
              {t('pages.admin.toolkitTypes.availability.enabled', 'Enabled')}
            </MenuItem>
            <MenuItem value="disabled">
              {t('pages.admin.toolkitTypes.availability.disabled', 'Disabled')}
            </MenuItem>
          </TextField>
        </Stack>
        <TextField
          fullWidth
          required
          multiline
          minRows={2}
          sx={{ marginTop: '1rem' }}
          label={t('pages.admin.toolkitTypes.reason.label', 'Reason')}
          value={reason}
          onChange={(event) => setReason(event.target.value)}
        />
        {failure === null ? null : (
          <Alert severity="error" sx={{ marginTop: '1rem' }} data-testid="admin-toolkit-types-grant-error">
            {failure}
          </Alert>
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={close} disabled={submitting}>
          {t('pages.admin.toolkitTypes.close', 'Close')}
        </Button>
        <Button
          variant="contained"
          disabled={!submittable}
          onClick={() => {
            void submit();
          }}
        >
          {t('pages.admin.toolkitTypes.action.grant', 'Add exception')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
