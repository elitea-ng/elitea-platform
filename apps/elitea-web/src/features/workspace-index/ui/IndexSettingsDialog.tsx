/**
 * A workspace's code index settings: turn it on (the code structure, parsed
 * on this computer, free), rebuild it, cancel a running build, turn it off
 * (the build is kept), or remove it (asks first). Embeddings come later and
 * show as such. When the policy turns the index off, the dialog says so
 * instead of offering controls.
 */
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import type { Theme } from '@mui/material/styles';
import Switch from '@mui/material/Switch';
import Typography from '@mui/material/Typography';

import type { IndexStatus } from '@/shared/desktop/indexIpc';
import { t } from '@/shared/i18n';

import { describeIndexError } from '../model/describeIndexError';
import type { IndexProgress } from '../model/progress';
import type { IndexAction, WorkspaceIndex } from '../model/useWorkspaceIndex';

export interface IndexSettingsDialogProps {
  open: boolean;
  onClose: () => void;
  /** The folder's name, for the title. */
  name: string;
  index: WorkspaceIndex;
}

const mutedSx = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });

function summary(status: IndexStatus, progress: IndexProgress | null): string {
  switch (status.state) {
    case 'off':
      return t('workspace.index.summary.off', 'The index is off for this folder.');
    case 'building':
      return progress === null || progress.total === null
        ? t('workspace.index.summary.building', 'Building the index…')
        : t('workspace.index.summary.buildingCount', 'Building the index: {{done}} of {{total}} files read.', { done: String(progress.done), total: String(progress.total) });
    case 'error':
      return status.error ?? t('workspace.index.chip.errorHint', 'The last build failed.');
    case 'ready':
    case 'stale':
      return t('workspace.index.summary.built', '{{files}} files, {{entities}} entities and {{relations}} relations.', {
        files: String(status.files),
        entities: String(status.entities),
        relations: String(status.relations),
      });
  }
}

function Details({ status, progress }: { status: IndexStatus; progress: IndexProgress | null }): React.JSX.Element {
  return (
    <Box data-testid="index-summary" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <Typography variant="bodySmall">{summary(status, progress)}</Typography>
      {status.last_run !== null && (
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.index.lastRun', 'Last built {{when}}.', { when: new Date(status.last_run).toLocaleString() })}
        </Typography>
      )}
      {status.state === 'stale' && status.changed_files > 0 && (
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.index.changedSince', '{{files}} files changed since; it refreshes when it is next used.', { files: String(status.changed_files) })}
        </Typography>
      )}
    </Box>
  );
}

function Embeddings(): React.JSX.Element {
  return (
    <Box component="section" data-testid="index-embeddings" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <Typography variant="labelMedium" component="h3" sx={{ margin: 0 }}>
        {t('workspace.index.embeddings.title', 'Semantic search')}
      </Typography>
      <FormControlLabel disabled control={<Switch size="small" checked={false} />} label={t('workspace.index.embeddings.toggle', 'Embeddings (coming soon)')} />
      <Typography variant="bodySmall" sx={mutedSx}>
        {t('workspace.index.embeddings.body', 'Searching by meaning needs embeddings from your deployment’s models. This is not available yet.')}
      </Typography>
    </Box>
  );
}

function Actions({ status, busy, run, onRemove }: { status: IndexStatus; busy: boolean; run: (action: IndexAction) => void; onRemove: () => void }): React.JSX.Element {
  const building = status.state === 'building';
  return (
    <>
      <Button color="error" disabled={busy} onClick={onRemove} sx={{ marginRight: 'auto' }}>
        {t('workspace.index.remove', 'Remove index…')}
      </Button>
      {status.state === 'off' ? (
        <Button variant="contained" disabled={busy} onClick={() => run('enable')}>
          {t('workspace.index.enable', 'Turn on')}
        </Button>
      ) : (
        <>
          <Button disabled={busy} onClick={() => run('disable')}>
            {t('workspace.index.disable', 'Turn off')}
          </Button>
          {building ? (
            <Button variant="contained" disabled={busy} onClick={() => run('cancel')}>
              {t('workspace.index.cancel', 'Cancel build')}
            </Button>
          ) : (
            <Button variant="contained" disabled={busy} onClick={() => run('rebuild')}>
              {t('workspace.index.rebuild', 'Rebuild')}
            </Button>
          )}
        </>
      )}
    </>
  );
}

interface FooterProps {
  status: IndexStatus;
  index: WorkspaceIndex;
  confirming: boolean;
  onConfirming: (confirming: boolean) => void;
  onClose: () => void;
}

/** The actions, or the remove confirmation's two answers. */
function Footer({ status, index, confirming, onConfirming, onClose }: FooterProps): React.JSX.Element {
  if (confirming) {
    return (
      <>
        <Button onClick={() => onConfirming(false)}>{t('workspace.index.keep', 'Keep it')}</Button>
        <Button
          variant="contained"
          color="error"
          disabled={index.pending}
          onClick={() => {
            index.run('remove');
            onConfirming(false);
          }}
        >
          {t('workspace.index.removeConfirmed', 'Remove')}
        </Button>
      </>
    );
  }
  return (
    <>
      <Actions status={status} busy={index.pending} run={index.run} onRemove={() => onConfirming(true)} />
      <Button onClick={onClose}>{t('workspace.index.close', 'Close')}</Button>
    </>
  );
}

export function IndexSettingsDialog({ open, onClose, name, index }: IndexSettingsDialogProps): React.JSX.Element {
  const [confirming, setConfirming] = useState(false);
  const { view, loadError, progress, actionError } = index;
  const close = (): void => {
    setConfirming(false);
    onClose();
  };
  const status = view?.kind === 'status' ? view.status : undefined;

  return (
    <Dialog open={open} onClose={close} aria-labelledby="workspace-index-title" fullWidth maxWidth="sm">
      <DialogTitle id="workspace-index-title">{t('workspace.index.title', 'Code index · {{name}}', { name })}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        {view?.kind === 'disabled' && (
          <Alert severity="info" data-testid="index-policy-off">
            {t('workspace.index.policyOff', 'The local code index is turned off by your organisation’s policy.')}
          </Alert>
        )}
        {view === undefined && loadError !== null && <Alert severity="error">{describeIndexError(loadError)}</Alert>}
        {status !== undefined && (
          <>
            <Typography variant="bodySmall" sx={mutedSx}>
              {t(
                'workspace.index.intro',
                'Agents can search this folder’s code structure (files, symbols and how they relate). It is parsed on this computer, kept in the app’s data, and costs nothing.',
              )}
            </Typography>
            <Details status={status} progress={progress} />
            <Embeddings />
          </>
        )}
        {confirming && (
          <Alert severity="warning" data-testid="index-remove-confirm">
            {t('workspace.index.removeConfirm', 'Remove this folder’s index? What was built is deleted; you can build it again.')}
          </Alert>
        )}
        {actionError !== null && <Alert severity="error">{describeIndexError(actionError)}</Alert>}
      </DialogContent>
      <DialogActions>
        {status === undefined ? (
          <Button onClick={close}>{t('workspace.index.close', 'Close')}</Button>
        ) : (
          <Footer status={status} index={index} confirming={confirming} onConfirming={setConfirming} onClose={close} />
        )}
      </DialogActions>
    </Dialog>
  );
}
