/**
 * The cloud chat's Unattended option (`useChatBoxUnattended`), above the
 * composer. Off: a "Run unattended…" button that opens the grant dialog. On:
 * a notice with the grant's summary (the person sees the grant before each
 * send), with Edit and Turn off. The dialog reuses the grant form with the
 * cloud parts: the step limit, the output limit, the sensitive tools of the
 * conversation's agent and the chain limits.
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

import type { ChatBoxUnattended as Mode } from './hooks/useChatBoxUnattended';

function GrantDialog({ unattended }: { readonly unattended: Mode }): React.JSX.Element {
  const { draft, sensitiveTools } = unattended;
  const valid = draft !== null && chainErrors(draft.chain).length === 0;
  return (
    <Dialog open={draft !== null} onClose={unattended.cancel} aria-labelledby="chat-unattended-title" maxWidth="sm" fullWidth>
      <DialogTitle id="chat-unattended-title">{t('workspace.unattended.title', 'Run unattended?')}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <DialogContentText>
          {t(
            'widgets.chatBox.unattended.body',
            'In unattended mode the agent runs the sensitive tools that you select without a pause, and it continues the run by itself as this grant allows. A question that the grant does not answer takes its default at once. Your organization’s guardrails, budgets and kill switches still apply.',
          )}
        </DialogContentText>
        {draft !== null && (
          <UnattendedGrantForm
            value={draft}
            onChange={unattended.setDraft}
            desktop={false}
            cloud
            {...(sensitiveTools === undefined ? {} : { sensitiveTools })}
          />
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={unattended.cancel}>{t('workspace.unattended.cancel', 'Cancel')}</Button>
        <Button color="warning" variant="contained" disabled={!valid} onClick={unattended.start} data-testid="chat-unattended-start">
          {t('workspace.unattended.start', 'Start')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

export function ChatBoxUnattendedControls({ unattended, disabled }: { readonly unattended: Mode; readonly disabled: boolean }): React.JSX.Element | null {
  const { grant } = unattended;
  if (!unattended.shown) return null;
  return (
    <>
      {grant === null ? (
        <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}>
          <Button size="small" color="inherit" disabled={disabled} onClick={unattended.open} data-testid="chat-unattended-open">
            {t('widgets.chatBox.unattended.open', 'Run unattended…')}
          </Button>
        </Box>
      ) : (
        <Alert
          severity="warning"
          data-testid="chat-unattended-notice"
          sx={{ mb: 1 }}
          action={
            <Box sx={{ display: 'flex', gap: 0.5 }}>
              <Button color="inherit" size="small" onClick={unattended.open}>
                {t('workspace.unattended.edit', 'Edit')}
              </Button>
              <Button color="inherit" size="small" onClick={unattended.turnOff}>
                {t('workspace.unattended.turnOff', 'Turn off')}
              </Button>
            </Box>
          }
        >
          {t('widgets.chatBox.unattended.notice', 'Unattended mode: each message that you send starts a run under this grant.')}
          <UnattendedGrantSummary value={grant} desktop={false} cloud />
        </Alert>
      )}
      <GrantDialog unattended={unattended} />
    </>
  );
}
