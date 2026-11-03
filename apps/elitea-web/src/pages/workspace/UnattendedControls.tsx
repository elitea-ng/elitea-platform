/**
 * Unattended mode's dialog and notice (`useUnattendedMode`): the dialog shows
 * the grant form before the mode turns on (Start keeps the grant for the
 * folder); the notice above the composer shows the grant while the mode is
 * on, with Edit and Turn off.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';

import { chainErrors, UnattendedGrantForm, UnattendedGrantSummary } from '@/features/unattended';
import { t } from '@/shared/i18n';

import type { UnattendedMode } from './useUnattendedMode';

function GrantDialog({ unattended }: { unattended: UnattendedMode }): React.JSX.Element {
  const { draft } = unattended;
  const valid = draft !== null && chainErrors(draft.chain).length === 0;
  return (
    <Dialog open={draft !== null} onClose={unattended.cancel} aria-labelledby="workspace-unattended-title" maxWidth="sm" fullWidth>
      <DialogTitle id="workspace-unattended-title">{t('workspace.unattended.title', 'Run unattended?')}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <DialogContentText>
          {t(
            'workspace.unattended.body',
            'In unattended mode the agent does not ask before it runs a command, changes a file or calls an MCP tool, and it continues the run by itself as this grant allows. A question that the grant does not answer takes its default at once. Your organization’s policy, your forbidden rules, budgets and kill switches still apply. Use it only in a folder that you can restore.',
          )}
        </DialogContentText>
        {draft !== null && <UnattendedGrantForm value={draft} onChange={unattended.setDraft} />}
      </DialogContent>
      <DialogActions>
        <Button onClick={unattended.cancel}>{t('workspace.unattended.cancel', 'Cancel')}</Button>
        <Button color="warning" variant="contained" disabled={!valid} onClick={unattended.start} data-testid="workspace-unattended-start">
          {t('workspace.unattended.start', 'Start')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

export function UnattendedControls({ unattended, active }: { unattended: UnattendedMode; active: boolean }): React.JSX.Element {
  const { grant } = unattended;
  return (
    <>
      {active && grant !== null && (
        <Alert
          severity="warning"
          data-testid="workspace-unattended-notice"
          action={
            <Box sx={{ display: 'flex', gap: 0.5 }}>
              <Button color="inherit" size="small" onClick={unattended.edit}>
                {t('workspace.unattended.edit', 'Edit')}
              </Button>
              <Button color="inherit" size="small" onClick={() => unattended.request(false)}>
                {t('workspace.unattended.turnOff', 'Turn off')}
              </Button>
            </Box>
          }
        >
          {t('workspace.unattended.notice', 'Unattended mode: the agent works without asking you and continues by itself.')}
          <UnattendedGrantSummary value={grant} />
        </Alert>
      )}
      <GrantDialog unattended={unattended} />
    </>
  );
}
