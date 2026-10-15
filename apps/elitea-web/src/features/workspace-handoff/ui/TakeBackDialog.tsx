/**
 * "Take back": get a thread running in the cloud back to this computer.
 * The dialog asks the cloud to hand it over, checks what came back against
 * your folder without touching it, and then offers what is safe: apply the
 * cloud's changes to your folder, open them as a separate worktree, or
 * discard them. Applying keeps a checkpoint; Undo puts the folder back.
 *
 * The review shows the per-file diffs of what the take back writes, through
 * `renderFiles` (the page passes the Workspace view's changed-files card,
 * read-only, as for a recorded turn: a feature does not import another);
 * a worktree result lists the excluded local files copied into it and says
 * it can be opened as a workspace.
 */
import { useEffect, useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { ApplyResult, HandoffChangedFile, HandoffIpc, Preflight, TakeBackMode } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

import { describeHandoffError } from '../model/describeHandoffError';
import { useTakeBack, type TakeBack } from '../model/useTakeBack';

export interface TakeBackDialogProps {
  open: boolean;
  /** Closed by the person; the take back may be unfinished (the thread stays as it is). */
  onClose: () => void;
  ipc: HandoffIpc;
  conversationUuid: string;
  /** The thread's state moved (released): read it and the history again. */
  onSettled: () => void;
  /** Renders the per-file diffs (read-only); without it, the changed paths are listed. `key` is unique per preflight. */
  renderFiles?: ((files: readonly HandoffChangedFile[], key: string) => ReactNode) | undefined;
}

type RenderFiles = TakeBackDialogProps['renderFiles'];

const mutedSx = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });
const PATHS_SHOWN = 20;

function Paths({ title, paths, testId }: { title: string; paths: readonly string[]; testId: string }): React.JSX.Element | null {
  if (paths.length === 0) return null;
  return (
    <Box component="section" data-testid={testId} sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <Typography variant="labelMedium" component="h3" sx={{ margin: 0 }}>
        {`${title} (${String(paths.length)})`}
      </Typography>
      <Box component="ul" sx={{ margin: 0, paddingLeft: 2.5 }}>
        {paths.slice(0, PATHS_SHOWN).map((path) => (
          <li key={path}>
            <Typography variant="bodySmall" sx={{ wordBreak: 'break-all' }}>
              {path}
            </Typography>
          </li>
        ))}
      </Box>
      {paths.length > PATHS_SHOWN && (
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.handoff.pathsMore', 'and {{n}} more', { n: String(paths.length - PATHS_SHOWN) })}
        </Typography>
      )}
    </Box>
  );
}

function Working({ label }: { label: string }): React.JSX.Element {
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }} data-testid="takeback-working">
      <CircularProgress size={16} />
      <Typography variant="bodyMedium" component="output">
        {label}
      </Typography>
    </Box>
  );
}

/** The diffs of what the take back writes; without a renderer (or diffs), the changed paths. */
function ChangedInCloud({ preflight, renderFiles }: { preflight: Preflight; renderFiles: RenderFiles }): React.JSX.Element | null {
  const title = t('workspace.handoff.paths.changed', 'Changed in the cloud');
  if (renderFiles === undefined || preflight.files.length === 0) return <Paths title={title} paths={preflight.changed_paths} testId="takeback-changed-paths" />;
  return (
    <Box data-testid="takeback-diffs" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      {renderFiles(preflight.files, `takeback-${preflight.preflight_id}`)}
      {preflight.diffs_truncated && (
        <Typography variant="bodySmall" data-testid="takeback-diffs-truncated" sx={mutedSx}>
          {t('workspace.handoff.review.truncated', 'Some diffs are not shown: they are too large to show here. The files are still written.')}
        </Typography>
      )}
    </Box>
  );
}

function Review({ preflight, confirmingDiscard, renderFiles }: { preflight: Preflight; confirmingDiscard: boolean; renderFiles: RenderFiles }): React.JSX.Element {
  const fast = preflight.outcome === 'fast_path';
  return (
    <>
      {fast ? (
        <Typography variant="bodyMedium">
          {t('workspace.handoff.review.fast', 'Your folder is as you left it. The cloud changed {{n}} files in {{turns}} turns.', {
            n: String(preflight.changed_paths.length),
            turns: String(preflight.cloud_turns),
          })}
        </Typography>
      ) : (
        <Alert severity="warning" data-testid="takeback-conflicts">
          {t(
            'workspace.handoff.review.conflicts',
            'Your folder changed since you sent this thread, so the cloud’s changes are not written into it. Open them as a separate worktree next to your folder, or discard them.',
          )}
        </Alert>
      )}
      <Paths title={t('workspace.handoff.paths.conflicts', 'Changed on both sides')} paths={preflight.conflict_paths} testId="takeback-conflict-paths" />
      <ChangedInCloud preflight={preflight} renderFiles={renderFiles} />
      <Paths title={t('workspace.handoff.paths.skipped', 'Never written or deleted here')} paths={preflight.skipped_paths} testId="takeback-skipped-paths" />
      {confirmingDiscard && (
        <Alert severity="warning" data-testid="takeback-discard-confirm">
          {t('workspace.handoff.discardConfirm', 'Discard the cloud’s changes? They are not kept anywhere on this computer. Your folder stays as it is.')}
        </Alert>
      )}
    </>
  );
}

function resultTitle(result: ApplyResult): string {
  switch (result.outcome) {
    case 'applied':
      return t('workspace.handoff.result.applied', 'The cloud’s changes are in your folder as uncommitted changes.');
    case 'worktree':
      return t('workspace.handoff.result.worktree', 'The cloud’s version is open as a separate worktree. Your folder was not changed.');
    case 'discarded':
      return t('workspace.handoff.result.discarded', 'The cloud’s changes were discarded. Your folder was not changed.');
  }
}

function Done({ preflight, result, undone }: { preflight: Preflight; result: ApplyResult; undone: boolean }): React.JSX.Element {
  return (
    <>
      <Alert severity="success" data-testid="takeback-result">
        {undone ? t('workspace.handoff.result.undone', 'Your folder is back to how it was before the take back.') : resultTitle(result)}
      </Alert>
      {result.outcome === 'applied' && !undone && <Paths title={t('workspace.handoff.paths.applied', 'Changed in your folder')} paths={preflight.changed_paths} testId="takeback-applied-paths" />}
      {result.outcome === 'applied' && result.branch !== null && (
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.handoff.result.branch', 'The cloud’s commits are on the branch {{branch}}.', { branch: result.branch })}
        </Typography>
      )}
      {result.outcome === 'worktree' && result.worktree_path !== null && (
        <Box data-testid="takeback-worktree" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
          <Typography variant="bodySmall" data-testid="takeback-worktree-path" sx={{ wordBreak: 'break-all' }}>
            {t('workspace.handoff.result.worktreePath', 'Worktree: {{path}}', { path: result.worktree_path })}
          </Typography>
          <Typography variant="bodySmall" sx={mutedSx}>
            {t('workspace.handoff.result.worktreeHint', 'Open this folder from the sidebar (Open folder) to work in it as a workspace.')}
          </Typography>
        </Box>
      )}
      {result.outcome === 'worktree' && (
        <Paths title={t('workspace.handoff.paths.copied', 'Copied from your folder into the worktree')} paths={result.copied} testId="takeback-copied-paths" />
      )}
      {result.imported_turns > 0 && (
        <Typography variant="bodySmall" sx={mutedSx}>
          {t('workspace.handoff.result.imported', '{{n}} cloud turns were added to this thread’s history here.', { n: String(result.imported_turns) })}
        </Typography>
      )}
      {!result.released && (
        <Alert severity="warning" data-testid="takeback-unreleased">
          {t('workspace.handoff.result.unreleased', 'The cloud has not confirmed the hand-back yet. It is sent again automatically; until then this thread stays read-only here.')}
        </Alert>
      )}
    </>
  );
}

function Body({ takeBack, confirmingDiscard, renderFiles }: { takeBack: TakeBack; confirmingDiscard: boolean; renderFiles: RenderFiles }): React.JSX.Element | null {
  const { step } = takeBack;
  switch (step.kind) {
    case 'idle':
      return null;
    case 'reclaiming':
      return <Working label={t('workspace.handoff.working.reclaiming', 'Asking the cloud to hand the thread back…')} />;
    case 'waiting':
      return <Working label={t('workspace.handoff.working.waiting', 'A cloud turn is still running. The take back continues when it ends (at most a minute after it is stopped).')} />;
    case 'preflighting':
      return <Working label={t('workspace.handoff.working.preflight', 'Checking the cloud’s changes against your folder…')} />;
    case 'applying':
      return <Working label={t('workspace.handoff.working.applying', 'Applying…')} />;
    case 'review':
      return <Review preflight={step.preflight} confirmingDiscard={confirmingDiscard} renderFiles={renderFiles} />;
    case 'done':
      return <Done preflight={step.preflight} result={step.result} undone={step.undone} />;
    case 'gone':
      return <Alert severity="info">{t('workspace.handoff.gone', 'This thread is no longer in the cloud, so there is nothing to take back.')}</Alert>;
  }
}

const MODE_ORDER: readonly TakeBackMode[] = ['apply', 'worktree', 'discard'];

function modeLabel(mode: TakeBackMode): string {
  switch (mode) {
    case 'apply':
      return t('workspace.handoff.mode.apply', 'Apply to my folder');
    case 'worktree':
      return t('workspace.handoff.mode.worktree', 'Open as worktree');
    case 'discard':
      return t('workspace.handoff.mode.discard', 'Discard cloud changes');
  }
}

interface ActionsProps {
  takeBack: TakeBack;
  confirmingDiscard: boolean;
  onConfirmDiscard: (confirming: boolean) => void;
  onClose: () => void;
}

function Actions({ takeBack, confirmingDiscard, onConfirmDiscard, onClose }: ActionsProps): React.JSX.Element {
  const { step } = takeBack;
  if (step.kind === 'idle') {
    return (
      <>
        <Button onClick={onClose}>{t('workspace.handoff.cancel', 'Cancel')}</Button>
        <Button variant="contained" onClick={takeBack.start}>
          {t('workspace.handoff.retry', 'Try again')}
        </Button>
      </>
    );
  }
  if (step.kind === 'review') {
    const modes = MODE_ORDER.filter((mode) => step.preflight.modes.includes(mode));
    const primary = modes.find((mode) => mode !== 'discard');
    return (
      <>
        <Button onClick={confirmingDiscard ? () => onConfirmDiscard(false) : onClose}>{confirmingDiscard ? t('workspace.handoff.back', 'Back') : t('workspace.handoff.later', 'Decide later')}</Button>
        {modes.map((mode) => {
          if (mode === 'discard') {
            return (
              <Button key={mode} color="error" onClick={() => (confirmingDiscard ? takeBack.apply('discard') : onConfirmDiscard(true))}>
                {confirmingDiscard ? t('workspace.handoff.discardNow', 'Discard') : modeLabel(mode)}
              </Button>
            );
          }
          if (confirmingDiscard) return null;
          return (
            <Button key={mode} variant={mode === primary ? 'contained' : 'text'} onClick={() => takeBack.apply(mode)}>
              {modeLabel(mode)}
            </Button>
          );
        })}
      </>
    );
  }
  if (step.kind === 'done') {
    return (
      <>
        {step.result.checkpoint_id !== null && !step.undone && (
          <Button disabled={takeBack.undoing} onClick={takeBack.undo} sx={{ marginRight: 'auto' }}>
            {t('workspace.handoff.undo', 'Undo')}
          </Button>
        )}
        <Button variant="contained" onClick={onClose}>
          {t('workspace.handoff.close', 'Close')}
        </Button>
      </>
    );
  }
  return (
    <Button onClick={onClose} disabled={step.kind === 'applying'}>
      {step.kind === 'gone' ? t('workspace.handoff.close', 'Close') : t('workspace.handoff.cancel', 'Cancel')}
    </Button>
  );
}

export function TakeBackDialog({ open, onClose, ipc, conversationUuid, onSettled, renderFiles }: TakeBackDialogProps): React.JSX.Element {
  const takeBack = useTakeBack(ipc, conversationUuid, onSettled);
  const [confirmingDiscard, setConfirmingDiscard] = useState(false);
  const { start } = takeBack;
  // Opening the dialog starts the take back: it is what the button asked for.
  useEffect(() => {
    if (open) start();
  }, [open, start]);
  const busy = takeBack.step.kind === 'applying';
  return (
    <Dialog open={open} onClose={busy ? undefined : onClose} aria-labelledby="handoff-takeback-title" fullWidth maxWidth="sm">
      <DialogTitle id="handoff-takeback-title">{t('workspace.handoff.takeBackTitle', 'Take this thread back')}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <Body takeBack={takeBack} confirmingDiscard={confirmingDiscard} renderFiles={renderFiles} />
        {takeBack.error !== null && takeBack.error !== undefined && (
          <Alert severity="error" data-testid="takeback-error">
            {describeHandoffError(takeBack.error)}
          </Alert>
        )}
      </DialogContent>
      <DialogActions>
        <Actions takeBack={takeBack} confirmingDiscard={confirmingDiscard} onConfirmDiscard={setConfirmingDiscard} onClose={onClose} />
      </DialogActions>
    </Dialog>
  );
}
