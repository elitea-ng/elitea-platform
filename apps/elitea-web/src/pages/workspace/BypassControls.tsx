/**
 * Bypass mode's warning and notice (`useBypassMode`): the confirmation the
 * first time a folder turns it on, and the line above the composer while it
 * is on, with a button that turns it off.
 */
import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';

import { t } from '@/shared/i18n';

import type { BypassMode } from './useBypassMode';

export function BypassControls({ bypass, active }: { bypass: BypassMode; active: boolean }): React.JSX.Element {
  return (
    <>
      {active && (
        <Alert
          severity="warning"
          data-testid="workspace-bypass-notice"
          action={
            <Button color="inherit" size="small" onClick={() => bypass.request(false)}>
              {t('workspace.bypass.turnOff', 'Turn off')}
            </Button>
          }
        >
          {t('workspace.bypass.notice', 'Bypass mode: the agent runs commands and edits files without asking you.')}
        </Alert>
      )}
      <Dialog open={bypass.confirming} onClose={bypass.cancel} aria-labelledby="workspace-bypass-title">
        <DialogTitle id="workspace-bypass-title">{t('workspace.bypass.title', 'Run without approvals?')}</DialogTitle>
        <DialogContent>
          <DialogContentText>
            {t(
              'workspace.bypass.body',
              'In bypass mode the agent does not ask before it runs a command, changes a file or calls an MCP tool. This includes commands that run outside the sandbox. Your organization’s policy, your forbidden rules and your hooks still apply. Use it only in a folder that you can restore.',
            )}
          </DialogContentText>
        </DialogContent>
        <DialogActions>
          <Button onClick={bypass.cancel}>{t('workspace.bypass.cancel', 'Cancel')}</Button>
          <Button color="warning" variant="contained" onClick={bypass.confirm}>
            {t('workspace.bypass.confirm', 'Turn on bypass mode')}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}
