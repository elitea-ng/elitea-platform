/**
 * "Send to cloud": what would leave this computer, before anything does.
 * Files and sizes, what is left out and why (grouped by who decided it:
 * the platform, the project, your folder's own settings, the organisation's
 * hard deny list), the secret findings (a send is blocked until each file is
 * left on the laptop or edited; there is no override), "Include anyway" for a
 * left-out file the policy lets you include (persisted in the folder's
 * settings; its findings are warnings this send must confirm), the
 * environment variables you chose to send, the note that the platform can
 * read what is uploaded, then the send with live progress and its Cancel.
 * Refusals show their figures: the caps breakdown, the findings, the quota.
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

import type { HandoffEvent, HandoffIpc, HandoffPreview, HandoffSent } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

import { capsOf, describeHandoffError, describeTooLarge, isCancelled, quotaOf } from '../model/describeHandoffError';
import { formatBytes } from '../model/format';
import { useSendToCloud, type SendToCloud } from '../model/useSendToCloud';
import { CapsList } from './CapsList';
import { FindingsList } from './FindingsList';
import { EnvSection, Excluded, Included, IncludeWarnings } from './SendToCloudSections';


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
const listSx = { margin: 0, paddingLeft: 2.5 } as const;
const sectionSx = { display: 'flex', flexDirection: 'column', gap: 0.5 } as const;

function Summary({ preview }: { preview: HandoffPreview }): React.JSX.Element {
  return (
    <Box data-testid="handoff-summary" sx={sectionSx}>
      <Typography variant="bodyMedium">
        {t('workspace.handoff.summary', '{{files}} files · {{size}} to upload · {{unpacked}} unpacked', {
          files: preview.files.toLocaleString(),
          size: formatBytes(preview.bytes),
          unpacked: formatBytes(preview.unpacked_bytes),
        })}
      </Typography>
      {preview.project_workspace_bytes_left !== null && (
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.handoff.quotaLeft', '{{size}} left in this project’s cloud storage', { size: formatBytes(preview.project_workspace_bytes_left) })}
        </Typography>
      )}
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

const PHASE_LABEL: Record<HandoffEvent['phase'], () => string> = {
  snapshot: () => t('workspace.handoff.phase.snapshot', 'Reading your folder'),
  scan: () => t('workspace.handoff.phase.scan', 'Checking for secrets'),
  bundle: () => t('workspace.handoff.phase.bundle', 'Packing the files'),
  begin: () => t('workspace.handoff.phase.begin', 'Starting the handoff on the server'),
  upload: () => t('workspace.handoff.phase.upload', 'Uploading'),
  publish: () => t('workspace.handoff.phase.publish', 'Publishing'),
  complete: () => t('workspace.handoff.phase.complete', 'Finishing'),
  cancelled: () => t('workspace.handoff.phase.cancelled', 'Cancelling'),
  error: () => t('workspace.handoff.phase.error', 'Something went wrong'),
};

function Progress({ event, cancelling }: { event: HandoffEvent | null; cancelling: boolean }): React.JSX.Element {
  const label = cancelling ? PHASE_LABEL.cancelled() : event === null ? t('workspace.handoff.phase.starting', 'Starting') : PHASE_LABEL[event.phase]();
  const determinate = !cancelling && event !== null && event.total > 0;
  return (
    <Box data-testid="handoff-progress" sx={sectionSx}>
      <Typography variant="bodyMedium" component="output">
        {!cancelling && event?.message ? `${label}: ${event.message}` : label}
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
          <Box component="ul" sx={listSx}>
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

const present = (error: unknown): boolean => error !== null && error !== undefined;

/** What the preview says before the lists: the platform can read it, the policy's source, caps, blocking findings, include warnings. */
function Notices({ state, preview, busy }: { state: SendToCloud; preview: HandoffPreview; busy: boolean }): React.JSX.Element {
  const { findings } = state;
  const overCaps = preview.caps.violations.length > 0;
  return (
    <>
      <Alert severity="info" data-testid="handoff-platform-note">
        {t('workspace.handoff.platformCanRead', 'The platform can read the files you upload. Secrets and key files are never sent.')}
      </Alert>
      {preview.policy.source === 'defaults' && (
        <Alert severity="info" data-testid="handoff-policy-defaults">
          {t(
            'workspace.handoff.policyDefaults',
            'The server did not provide its handoff policy, so the default limits and exclusions are shown. The server checks its own when you send.',
          )}
        </Alert>
      )}
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
            <FindingsList findings={findings} disabled={busy} onLeave={state.leaveOnLaptop} />
          </Box>
        </Alert>
      )}
      {preview.include_findings.length > 0 && (
        <IncludeWarnings findings={preview.include_findings} confirmed={state.includesConfirmed} onConfirm={state.confirmIncludes} disabled={busy} />
      )}
    </>
  );
}

function Body({ state }: { state: SendToCloud }): React.JSX.Element {
  const { preview, previewError, loading, sending, saving } = state;
  if (present(previewError) && preview === undefined) return <Refusal error={previewError} />;
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
  const busy = sending || loading || saving;
  return (
    <>
      <Typography variant="bodySmall" sx={mutedSx}>
        {t('workspace.handoff.intro', 'The thread keeps going in the cloud while you are away, working on a copy of this folder. You can take it back at any time.')}
      </Typography>
      <Summary preview={preview} />
      <Notices state={state} preview={preview} busy={busy} />
      {present(state.settingsError) && (
        <Alert severity="error" data-testid="handoff-settings-error">
          {describeHandoffError(state.settingsError)}
        </Alert>
      )}
      <Included included={preview.included} disabled={busy} onRemove={state.removeInclude} />
      <Excluded preview={preview} disabled={busy} onInclude={state.include} />
      {preview.env !== null && <EnvSection env={preview.env} preview={preview} disabled={busy} onToggle={state.toggleEnv} />}
      <SendOutcome state={state} />
    </>
  );
}

/** The send's progress while it runs; afterwards, its cancellation (not an error) or its refusal. */
function SendOutcome({ state }: { state: SendToCloud }): React.JSX.Element | null {
  const { sending, sendError, progress } = state;
  if (sending) return <Progress event={progress} cancelling={state.cancelling} />;
  if (!present(sendError)) return null;
  if (isCancelled(sendError)) {
    return (
      <Alert severity="info" data-testid="handoff-send-cancelled">
        {t('workspace.handoff.cancelled', 'Sending was cancelled; the thread stays on this computer.')}
      </Alert>
    );
  }
  return <Refusal error={sendError} />;
}

/** Why Send is off: nothing to send yet, a cap or finding in the way, an unconfirmed include, an env over the limit. */
function blocked(state: SendToCloud): boolean {
  const { preview } = state;
  if (preview === undefined || state.loading || state.saving) return true;
  if (preview.caps.violations.length > 0 || state.findings.length > 0) return true;
  if (preview.include_findings.length > 0 && !state.includesConfirmed) return true;
  return preview.env?.over_limit === true;
}

export function SendToCloudDialog({ open, onClose, ipc, workspaceId, conversationUuid, onSent }: SendToCloudDialogProps): React.JSX.Element {
  const state = useSendToCloud(ipc, workspaceId, conversationUuid, onSent);
  const { sending } = state;
  return (
    <Dialog open={open} onClose={sending ? undefined : onClose} aria-labelledby="handoff-send-title" fullWidth maxWidth="sm">
      <DialogTitle id="handoff-send-title">{t('workspace.handoff.sendTitle', 'Send this thread to the cloud')}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <Body state={state} />
      </DialogContent>
      <DialogActions>
        {sending ? (
          <Button disabled={state.cancelling} onClick={state.cancel}>
            {t('workspace.handoff.cancel', 'Cancel')}
          </Button>
        ) : (
          <Button onClick={onClose}>{t('workspace.handoff.cancel', 'Cancel')}</Button>
        )}
        <Button variant="contained" disabled={blocked(state) || sending} onClick={state.send}>
          {t('workspace.handoff.send', 'Send to cloud')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
