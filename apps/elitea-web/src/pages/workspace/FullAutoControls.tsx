/**
 * Full auto's confirmation and banner (`useFullAutoMode`, issue 3079): the
 * dialog every time a thread turns it on, which names the risk, and the red
 * line above the composer while it is on. The line cannot be dismissed; its
 * button turns the mode off.
 */
import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';

import { t } from '@/shared/i18n';

import type { FullAutoMode } from './useFullAutoMode';

export function FullAutoControls({ fullAuto, active }: { fullAuto: FullAutoMode; active: boolean }): React.JSX.Element {
  return (
    <>
      {active && (
        <Alert
          severity="error"
          variant="filled"
          data-testid="workspace-full-auto-banner"
          action={
            <Button color="inherit" size="small" onClick={() => fullAuto.request(false)}>
              {t('workspace.fullAuto.turnOff', 'Turn off')}
            </Button>
          }
        >
          {t('workspace.fullAuto.banner', 'Full auto: the agent answers every approval, including questions after reading outside content.')}
        </Alert>
      )}
      <Dialog open={fullAuto.confirming} onClose={fullAuto.cancel} aria-labelledby="workspace-full-auto-title">
        <DialogTitle id="workspace-full-auto-title">{t('workspace.fullAuto.title', 'Run this thread in Full auto?')}</DialogTitle>
        <DialogContent>
          <DialogContentText sx={{ marginBottom: 1 }}>
            {t(
              'workspace.fullAuto.risk',
              'Full auto answers every approval for you, including the questions asked after the agent read content from outside this folder. Instructions hidden in a web page, an issue, command output or another agent’s reply can then make the agent run commands, outside the sandbox in a trusted folder, without asking you.',
            )}
          </DialogContentText>
          <DialogContentText sx={{ marginBottom: 1 }}>
            {t(
              'workspace.fullAuto.stillAsks',
              'Still asked: recursive deletes outside the folder, commands that touch credential stores, shell profiles or this app’s settings, computer use and purchases in the browser. When nobody is there to answer, they are declined. Your organization’s policy, your forbidden rules and your hooks still apply.',
            )}
          </DialogContentText>
          <DialogContentText>
            {t('workspace.fullAuto.scope', 'It applies to this thread only, until you turn it off or close the app. Agents that another agent hands work to do not get it.')}
          </DialogContentText>
        </DialogContent>
        <DialogActions>
          <Button onClick={fullAuto.cancel}>{t('workspace.fullAuto.cancel', 'Cancel')}</Button>
          <Button color="error" variant="contained" onClick={fullAuto.confirm} data-testid="workspace-full-auto-confirm">
            {t('workspace.fullAuto.confirm', 'Turn on Full auto')}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}
