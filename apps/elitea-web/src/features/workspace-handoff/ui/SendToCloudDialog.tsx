/**
 * "Send to cloud": what would leave this computer, before anything does.
 * Files and sizes, what is left out and why, the secret findings (a send is
 * blocked until each file is left on the laptop or edited; there is no
 * override), the note that the platform can read what is uploaded, then the
 * send with live progress. Refusals show their figures: the caps breakdown,
 * the findings, the quota.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import LinearProgress from '@mui/material/LinearProgress';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { ExcludeReason, HandoffEvent, HandoffIpc, HandoffPreview, HandoffSent } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

import { capsOf, describeHandoffError, describeTooLarge, quotaOf } from '../model/describeHandoffError';
import { formatBytes } from '../model/format';
import { useSendToCloud, type SendToCloud } from '../model/useSendToCloud';
import { CapsList } from './CapsList';
import { FindingsList } from './FindingsList';

export interface SendToCloudDialogProps {
  open: boolean;
  onClose: () => void;
  ipc: HandoffIpc;
  workspaceId: string;
  conversationUuid: string;
  /** The thread is in the cloud now. */
  onSent: (sent: HandoffSent) => void;
}

const mutedSx = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });
const EXCLUDED_SHOWN = 8;

function reasonLabel(reason: ExcludeReason): string {
  switch (reason) {
    case 'mandatory':
      return t('workspace.handoff.reason.mandatory', 'never sent (secrets and keys)');
    case 'path_deny':
      return t('workspace.handoff.reason.pathDeny', 'blocked by your organisation');
    case 'eliteaignore':
      return t('workspace.handoff.reason.eliteaignore', 'listed in .eliteaignore');
    case 'leave_on_laptop':
      return t('workspace.handoff.reason.leave', 'left on your laptop');
    case 'not_a_file':
      return t('workspace.handoff.reason.notAFile', 'not a regular file');
  }
}

function Summary({ preview }: { preview: HandoffPreview }): React.JSX.Element {
  return (
    <Box data-testid="handoff-summary" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <Typography variant="bodyMedium">
        {t('workspace.handoff.summary', '{{files}} files · {{size}} to upload · {{unpacked}} unpacked', {
          files: preview.files.toLocaleString(),
          size: formatBytes(preview.bytes),
          unpacked: formatBytes(preview.unpacked_bytes),
        })}
      </Typography>
      <Typography variant="bodySmall" sx={mutedSx}>
        {t('workspace.handoff.history', 'History: none. Only the files as they are now are sent, not your git history.')}
      </Typography>
      {preview.shadow && (
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.handoff.shadow', 'This folder is not a git repository, so Elitea tracks its changes in a private copy kept in the app’s data.')}
        </Typography>
      )}
    </Box>
  );
}

function Excluded({ excluded }: { excluded: HandoffPreview['excluded'] }): React.JSX.Element | null {
  if (excluded.length === 0) return null;
  return (
    <Box component="section" data-testid="handoff-excluded" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <Typography variant="labelMedium" component="h3" sx={{ margin: 0 }}>
        {t('workspace.handoff.excludedTitle', 'Left out ({{n}})', { n: String(excluded.length) })}
      </Typography>
      <Box component="ul" sx={{ margin: 0, paddingLeft: 2.5 }}>
        {excluded.slice(0, EXCLUDED_SHOWN).map((entry) => (
          <li key={entry.path}>
            <Typography variant="bodySmall">{`${entry.path} · ${reasonLabel(entry.reason)}`}</Typography>
          </li>
        ))}
      </Box>
      {excluded.length > EXCLUDED_SHOWN && (
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.handoff.excludedMore', 'and {{n}} more', { n: String(excluded.length - EXCLUDED_SHOWN) })}
        </Typography>
      )}
    </Box>
  );
}

const PHASE_LABEL: Record<HandoffEvent['phase'], () => string> = {
  snapshot: () => t('workspace.handoff.phase.snapshot', 'Reading your folder'),
  scan: () => t('workspace.handoff.phase.scan', 'Checking for secrets'),
  bundle: () => t('workspace.handoff.phase.bundle', 'Packing the files'),
  upload: () => t('workspace.handoff.phase.upload', 'Uploading'),
  publish: () => t('workspace.handoff.phase.publish', 'Publishing'),
  complete: () => t('workspace.handoff.phase.complete', 'Finishing'),
  error: () => t('workspace.handoff.phase.error', 'Something went wrong'),
};

function Progress({ event }: { event: HandoffEvent | null }): React.JSX.Element {
  const label = event === null ? t('workspace.handoff.phase.starting', 'Starting') : PHASE_LABEL[event.phase]();
  const determinate = event !== null && event.total > 0;
  return (
    <Box data-testid="handoff-progress" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <Typography variant="bodyMedium" component="output">
        {event?.message ? `${label}: ${event.message}` : label}
      </Typography>
      <LinearProgress
        variant={determinate ? 'determinate' : 'indeterminate'}
        value={determinate ? Math.min(100, Math.round((event.done / event.total) * 100)) : 0}
        aria-label={label}
      />
    </Box>
  );
}

function Refusal({ error }: { error: unknown }): React.JSX.Element {
  const caps = capsOf(error);
  const quota = quotaOf(error);
  return (
    <Alert severity="error" data-testid="handoff-send-error">
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
        <Typography variant="bodyMedium">{describeHandoffError(error)}</Typography>
        {caps !== undefined && <CapsList caps={caps} />}
        {quota.length > 0 && (
          <Box component="ul" sx={{ margin: 0, paddingLeft: 2.5 }}>
            {quota.map((figure) => (
              <li key={figure.name}>
                <Typography variant="bodySmall">{`${figure.name.replaceAll('_', ' ')}: ${formatBytes(figure.value)}`}</Typography>
              </li>
            ))}
          </Box>
        )}
      </Box>
    </Alert>
  );
}

function Body({ state }: { state: SendToCloud }): React.JSX.Element {
  const { preview, previewError, loading, findings, sending, sendError, progress } = state;
  if (previewError !== null && previewError !== undefined && preview === undefined) return <Refusal error={previewError} />;
  if (preview === undefined) {
    return (
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }} data-testid="handoff-loading">
        <CircularProgress size={16} />
        <Typography variant="bodySmall" component="output">
          {t('workspace.handoff.checking', 'Checking what would be sent…')}
        </Typography>
      </Box>
    );
  }
  const overCaps = preview.caps.violations.length > 0;
  return (
    <>
      <Typography variant="bodySmall" sx={mutedSx}>
        {t('workspace.handoff.intro', 'The thread keeps going in the cloud while you are away, working on a copy of this folder. You can take it back at any time.')}
      </Typography>
      <Summary preview={preview} />
      <Alert severity="info" data-testid="handoff-platform-note">
        {t('workspace.handoff.platformCanRead', 'The platform can read the files you upload. Secrets and key files are never sent.')}
      </Alert>
      {overCaps && (
        <Alert severity="error" data-testid="handoff-caps-error">
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
            <Typography variant="bodyMedium">{describeTooLarge()}</Typography>
            <CapsList caps={preview.caps} />
          </Box>
        </Alert>
      )}
      {findings.length > 0 && (
        <Alert severity="warning" data-testid="handoff-findings-alert">
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
            <Typography variant="bodyMedium">
              {t('workspace.handoff.findingsTitle', 'These files look like they hold secrets. Leave them on your laptop, or edit them, to send.')}
            </Typography>
            <FindingsList findings={findings} disabled={sending || loading} onLeave={state.leaveOnLaptop} />
          </Box>
        </Alert>
      )}
      <Excluded excluded={preview.excluded} />
      {sending && <Progress event={progress} />}
      {!sending && sendError !== null && sendError !== undefined && <Refusal error={sendError} />}
    </>
  );
}

export function SendToCloudDialog({ open, onClose, ipc, workspaceId, conversationUuid, onSent }: SendToCloudDialogProps): React.JSX.Element {
  const state = useSendToCloud(ipc, workspaceId, conversationUuid, onSent);
  const { preview, findings, sending, loading } = state;
  const blocked = preview === undefined || preview.caps.violations.length > 0 || findings.length > 0 || loading;
  return (
    <Dialog open={open} onClose={sending ? undefined : onClose} aria-labelledby="handoff-send-title" fullWidth maxWidth="sm">
      <DialogTitle id="handoff-send-title">{t('workspace.handoff.sendTitle', 'Send this thread to the cloud')}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <Body state={state} />
      </DialogContent>
      <DialogActions>
        <Button disabled={sending} onClick={onClose}>
          {t('workspace.handoff.cancel', 'Cancel')}
        </Button>
        <Button variant="contained" disabled={blocked || sending} onClick={state.send}>
          {t('workspace.handoff.send', 'Send to cloud')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
