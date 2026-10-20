/**
 * A workspace's code index settings: turn it on (the code structure, parsed
 * on this computer, free), refresh it (only what changed is read) or rebuild
 * it (everything is read again), cancel a running build, turn it off (the
 * build is kept), or remove it (asks first), and choose how it searches by
 * meaning (`EmbeddingsSection`). When the policy turns the index off, the dialog says so
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
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { IndexStatus } from '@/shared/desktop/indexIpc';
import { t } from '@/shared/i18n';

import { describeIndexError } from '../model/describeIndexError';
import type { IndexProgress } from '../model/progress';
import type { WorkspaceEmbeddings } from '../model/useEmbeddings';
import type { IndexAction, WorkspaceIndex } from '../model/useWorkspaceIndex';
import { EmbeddingsSection } from './EmbeddingsSection';

export interface IndexSettingsDialogProps {
  open: boolean;
  onClose: () => void;
  /** The folder's name, for the title. */
  name: string;
  index: WorkspaceIndex;
  /** Semantic search, once the index is on. */
  embeddings?: WorkspaceEmbeddings;
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
    case 'stale_policy':
      return t('workspace.index.summary.stalePolicy', 'Your organisation’s file access rules changed since this index was built. Agents get it again once it is rebuilt under the new rules.');
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
          {t('workspace.index.changedSince', '{{files}} files changed since; it refreshes a few seconds after the agent’s changes.', { files: String(status.changed_files) })}
        </Typography>
      )}
      {status.state === 'stale' && status.changed_files === 0 && (
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.index.notChecked', 'Not checked since the app started; it is checked when you open this folder.')}
        </Typography>
      )}
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
            <>
              <Button disabled={busy} onClick={() => run('rebuild')}>
                {t('workspace.index.rebuild', 'Rebuild')}
              </Button>
              <Button variant="contained" disabled={busy} onClick={() => run('refresh')}>
                {t('workspace.index.refresh', 'Refresh')}
              </Button>
            </>
          )}
        </>
      )}
    </>
  );
}

/** The policy turns the index off, but one is on this computer: it may still be turned off or removed. */
function PolicyOffActions({ busy, run, onRemove }: { busy: boolean; run: (action: IndexAction) => void; onRemove: () => void }): React.JSX.Element {
  return (
    <>
      <Button color="error" disabled={busy} onClick={onRemove} sx={{ marginRight: 'auto' }}>
        {t('workspace.index.remove', 'Remove index…')}
      </Button>
      <Button disabled={busy} onClick={() => run('disable')}>
        {t('workspace.index.disable', 'Turn off')}
      </Button>
    </>
  );
}

/** What the footer offers: the controls, the two still allowed under a policy that turns the index off, or nothing but Close. */
function footerActions(index: WorkspaceIndex, onRemove: () => void): React.ReactNode {
  const { view } = index;
  if (view?.kind === 'status') return <Actions status={view.status} busy={index.pending} run={index.run} onRemove={onRemove} />;
  if (view?.kind === 'disabled' && view.onDisk) return <PolicyOffActions busy={index.pending} run={index.run} onRemove={onRemove} />;
  return null;
}

interface FooterProps {
  /** The actions offered (not while confirming a removal). */
  actions: React.ReactNode;
  index: WorkspaceIndex;
  confirming: boolean;
  onConfirming: (confirming: boolean) => void;
  onClose: () => void;
}

/** The actions, or the remove confirmation's two answers. */
function Footer({ actions, index, confirming, onConfirming, onClose }: FooterProps): React.JSX.Element {
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
      {actions}
      <Button onClick={onClose}>{t('workspace.index.close', 'Close')}</Button>
    </>
  );
}

/** What a folder's index holds, and its semantic search once it is on. */
function Built({ status, progress, embeddings }: { status: IndexStatus; progress: IndexProgress | null; embeddings: WorkspaceEmbeddings | undefined }): React.JSX.Element {
  return (
    <>
      <Typography variant="bodySmall" sx={mutedSx}>
        {t(
          'workspace.index.intro',
          'Agents can search this folder’s code structure (files, symbols and how they relate). It is parsed on this computer, kept in the app’s data, and costs nothing.',
        )}
      </Typography>
      <Details status={status} progress={progress} />
      {embeddings !== undefined && status.state !== 'off' && <EmbeddingsSection embeddings={embeddings} chunks={status.chunks} />}
    </>
  );
}

interface BodyProps {
  index: WorkspaceIndex;
  confirming: boolean;
  embeddings?: WorkspaceEmbeddings;
}

/** Everything the settings say, without a frame: the dialog puts it in its content, the inspector in its pane. */
function Body({ index, confirming, embeddings }: BodyProps): React.JSX.Element {
  const { view, loadError, progress, actionError } = index;
  const status = view?.kind === 'status' ? view.status : undefined;
  return (
    <>
      {view?.kind === 'disabled' && (
        <Alert severity="info" data-testid="index-policy-off">
          {t('workspace.index.policyOff', 'The local code index is turned off by your organisation’s policy.')}
        </Alert>
      )}
      {view === undefined && loadError !== null && <Alert severity="error">{describeIndexError(loadError)}</Alert>}
      {status !== undefined && <Built status={status} progress={progress} embeddings={embeddings} />}
      {confirming && (
        <Alert severity="warning" data-testid="index-remove-confirm">
          {t('workspace.index.removeConfirm', 'Remove this folder’s index? What was built is deleted; you can build it again.')}
        </Alert>
      )}
      {actionError !== null && <Alert severity="error">{describeIndexError(actionError)}</Alert>}
    </>
  );
}

export function IndexSettingsDialog({ open, onClose, name, index, embeddings }: IndexSettingsDialogProps): React.JSX.Element {
  const [confirming, setConfirming] = useState(false);
  const close = (): void => {
    setConfirming(false);
    onClose();
  };
  const actions = footerActions(index, () => setConfirming(true));

  return (
    <Dialog open={open} onClose={close} aria-labelledby="workspace-index-title" fullWidth maxWidth="sm">
      <DialogTitle id="workspace-index-title">{t('workspace.index.title', 'Code index · {{name}}', { name })}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <Body index={index} confirming={confirming} embeddings={embeddings} />
      </DialogContent>
      <DialogActions>
        {actions === null ? (
          <Button onClick={close}>{t('workspace.index.close', 'Close')}</Button>
        ) : (
          <Footer actions={actions} index={index} confirming={confirming} onConfirming={setConfirming} onClose={close} />
        )}
      </DialogActions>
    </Dialog>
  );
}

/** The same settings as a pane (the inspector's Index segment): the actions sit under the text, no Close. */
export function IndexSettingsPanel({ index, embeddings }: { index: WorkspaceIndex; embeddings?: WorkspaceEmbeddings }): React.JSX.Element {
  const [confirming, setConfirming] = useState(false);
  const actions = footerActions(index, () => setConfirming(true));
  return (
    <Box data-testid="index-panel" sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Body index={index} confirming={confirming} embeddings={embeddings} />
      {actions !== null && (
        <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1, alignItems: 'center' }}>
          {confirming ? <Footer actions={actions} index={index} confirming onConfirming={setConfirming} onClose={() => setConfirming(false)} /> : actions}
        </Box>
      )}
    </Box>
  );
}
