/**
 * Merge a parallel task's work back into the main folder. It opens on the
 * host's dry run (`worktree_merge_preflight`, writes nothing): what applying
 * would write or delete, what is never written (skipped paths), and, when
 * both sides changed the same files, the conflicts. A conflicting merge is
 * not applied as it is: it offers "Apply with conflict markers" (the same
 * mode the take-back offers), or the work stays on its branch. Each mode
 * button sends the preview's id with its mode (the preview's `modes`);
 * the host checkpoints the main folder first, so the result offers "Undo"
 * (`worktree_merge_undo`). A preview the folder has outgrown is taken again. When the folder was checkpointed
 * after the merge (a later turn), the undo would revert that too: the host refuses it, the host's reason is
 * shown, and "Undo anyway" asks for it.
 */
import { useMutation, useQuery } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';

import type { MergeApplied, MergeApplyMode, MergePreflight, WorktreeEntry, WorktreeIpc } from '@/shared/desktop/worktreeIpc';
import { t } from '@/shared/i18n';

import { describeWorktreeError, needsNewPreview, undoNeedsConfirm } from '../model/describeWorktreeError';

export interface MergeBackDialogProps {
  ipc: WorktreeIpc;
  workspaceId: string;
  entry: WorktreeEntry;
  onClose: () => void;
  /** The main folder or the task changed (applied or undone): the list re-reads its counts. */
  onChanged: () => void;
}

function PathList({ title, paths, testId }: { title: string; paths: readonly string[]; testId: string }): React.JSX.Element | null {
  if (paths.length === 0) return null;
  return (
    <Box data-testid={testId}>
      <Typography variant="labelMedium" component="p">
        {title}
      </Typography>
      <Box component="ul" sx={{ margin: 0, paddingLeft: 3, maxHeight: '10rem', overflowY: 'auto' }}>
        {paths.map((path) => (
          <li key={path}>
            <Typography variant="bodySmall" component="span" sx={{ fontFamily: 'monospace' }}>
              {path}
            </Typography>
          </li>
        ))}
      </Box>
    </Box>
  );
}

function modeText(preflight: MergePreflight): string {
  const n = preflight.applied_paths.length;
  switch (preflight.mode) {
    case 'fast_forward':
      return t('workspace.worktree.mode.fastForward', 'The main folder has not changed since this task started: {{n}} files are brought over as they are.', { n });
    case 'merge':
      return t('workspace.worktree.mode.merge', 'Both sides changed, but not the same files: {{n}} files are merged into the main folder.', { n });
    case 'conflicts':
      return t(
        'workspace.worktree.mode.conflicts',
        'Both sides changed the same files. Apply with conflict markers writes the merge with markers to resolve in those files (a binary file keeps the version in the main folder); otherwise the work stays on its branch, {{branch}}.',
        { branch: preflight.branch },
      );
    case 'nothing':
      return t('workspace.worktree.mode.nothing', 'There is nothing on this branch to merge back.');
  }
}

/** The conflicting text files a markers apply leaves markers in (a skipped path keeps the folder's file). */
function markerPaths(preflight: MergePreflight): string[] {
  const skipped = new Set(preflight.skipped_paths);
  return preflight.conflict_paths.filter((path) => !skipped.has(path));
}

const MODE_ORDER: readonly MergeApplyMode[] = ['apply', 'apply_with_markers'];

function modeLabel(mode: MergeApplyMode): string {
  return mode === 'apply' ? t('workspace.worktree.apply', 'Apply') : t('workspace.worktree.applyWithMarkers', 'Apply with conflict markers');
}

const SKIPPED_TITLE = (): string => t('workspace.worktree.skippedPaths', 'Never written (protected paths, links, submodules)');

/** The dry run: how the merge would go, and every path it touches or leaves alone. */
function PreviewBody({ preflight }: { preflight: MergePreflight }): React.JSX.Element {
  const conflicts = preflight.mode === 'conflicts';
  return (
    <>
      <Alert severity={conflicts ? 'warning' : 'info'} data-testid="worktree-merge-mode">
        {modeText(preflight)}
      </Alert>
      <PathList testId="worktree-conflict-paths" title={t('workspace.worktree.conflictPaths', 'Changed on both sides')} paths={preflight.conflict_paths} />
      <PathList testId="worktree-applied-paths" title={t('workspace.worktree.appliedPaths', 'Written or deleted in the main folder')} paths={preflight.applied_paths} />
      <PathList testId="worktree-skipped-paths" title={SKIPPED_TITLE()} paths={preflight.skipped_paths} />
    </>
  );
}

interface AppliedBodyProps {
  applied: MergeApplied;
  /** The conflicting paths written with markers, when the apply was a markers one. */
  markers: readonly string[];
  restored: readonly string[] | undefined;
  undoError: unknown;
}

/** What the apply wrote, and the undo it allows (from the checkpoint the host took first). */
function AppliedBody({ applied, markers, restored, undoError }: AppliedBodyProps): React.JSX.Element {
  return (
    <>
      <Alert severity="success" data-testid="worktree-merge-applied">
        {t('workspace.worktree.applied', 'Merged back: {{written}} files written, {{deleted}} deleted.', { written: applied.written.length, deleted: applied.deleted.length })}
      </Alert>
      <PathList testId="worktree-marker-paths" title={t('workspace.worktree.markerPaths', 'With conflict markers to resolve')} paths={markers} />
      <PathList testId="worktree-applied-skipped" title={SKIPPED_TITLE()} paths={applied.skipped_paths} />
      {applied.checkpoint_id !== null && restored === undefined && (
        <Typography variant="bodySmall">{t('workspace.worktree.checkpointNote', 'The main folder was checkpointed first, so this merge can be undone.')}</Typography>
      )}
      {restored !== undefined && (
        <Alert severity="info">{t('workspace.worktree.undone', 'Undone: {{n}} files restored to before the merge.', { n: restored.length })}</Alert>
      )}
      {undoError !== null && <Alert severity="error">{describeWorktreeError(undoError)}</Alert>}
    </>
  );
}

/** The preview, the apply and the undo of one merge back. */
function useMergeBack(ipc: WorktreeIpc, workspaceId: string, entry: WorktreeEntry, onChanged: () => void) {
  const preview = useQuery({
    queryKey: ['worktree', 'preflight', workspaceId, entry.thread_uuid],
    queryFn: () => ipc.preflight(workspaceId, entry.thread_uuid),
    staleTime: 0,
    gcTime: 0,
  });
  const apply = useMutation({
    mutationFn: ({ preflightId, mode }: { preflightId: string; mode: MergeApplyMode }) => ipc.apply(preflightId, mode),
    onSuccess: onChanged,
  });
  const undo = useMutation({
    mutationFn: ({ id, confirmOlder }: { id: string; confirmOlder: boolean }) => ipc.undo(id, confirmOlder ? true : undefined),
    onSuccess: onChanged,
  });
  const preflight = preview.data;
  return {
    preview,
    apply,
    undo,
    // The host's modes for this preview: none for `nothing`, only markers for conflicts.
    modes: MODE_ORDER.filter((mode) => preflight?.modes.includes(mode) === true),
    busy: preview.isFetching || apply.isPending || undo.isPending,
    checkAgain: (): void => {
      apply.reset();
      void preview.refetch();
    },
  };
}

/** Before the apply: Cancel, Check again (a stale preview) and Apply; after it: Close and Undo. */
function MergeActions({ merge, onClose }: { merge: ReturnType<typeof useMergeBack>; onClose: () => void }): React.JSX.Element {
  const { preview, apply, undo, modes, busy, checkAgain } = merge;
  const preflight = preview.data;
  const applied = apply.data;
  const checkpointId = applied?.checkpoint_id ?? null;
  const mayUndo = checkpointId !== null && undo.data === undefined;
  return (
    <DialogActions>
      <Button disabled={busy} onClick={onClose}>
        {applied === undefined ? t('workspace.cancel', 'Cancel') : t('workspace.worktree.close', 'Close')}
      </Button>
      {applied === undefined && needsNewPreview(apply.error) && (
        <Button disabled={busy} onClick={checkAgain}>
          {t('workspace.worktree.checkAgain', 'Check again')}
        </Button>
      )}
      {applied === undefined && modes.length === 0 && (
        <Button variant="contained" disabled>
          {t('workspace.worktree.apply', 'Apply')}
        </Button>
      )}
      {applied === undefined &&
        modes.map((mode, index) => (
          <Button
            key={mode}
            variant={index === 0 ? 'contained' : 'outlined'}
            disabled={busy}
            onClick={() => preflight !== undefined && apply.mutate({ preflightId: preflight.preflight_id, mode })}
          >
            {modeLabel(mode)}
          </Button>
        ))}
      {mayUndo && (
        <Button disabled={busy} onClick={() => undo.mutate({ id: checkpointId, confirmOlder: undoNeedsConfirm(undo.error) })}>
          {undoNeedsConfirm(undo.error) ? t('workspace.worktree.undoAnyway', 'Undo anyway') : t('workspace.worktree.undo', 'Undo merge')}
        </Button>
      )}
    </DialogActions>
  );
}

export function MergeBackDialog({ ipc, workspaceId, entry, onClose, onChanged }: MergeBackDialogProps): React.JSX.Element {
  const merge = useMergeBack(ipc, workspaceId, entry, onChanged);
  const { preview, apply, undo, busy } = merge;
  const preflight = preview.data;
  const applied = apply.data;

  return (
    <Dialog open onClose={busy ? undefined : onClose} aria-labelledby="worktree-merge-title" fullWidth maxWidth="sm">
      <DialogTitle id="worktree-merge-title">{t('workspace.worktree.mergeTitle', 'Merge {{branch}} back', { branch: entry.branch })}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        {preview.isPending && <Typography variant="bodyMedium">{t('workspace.worktree.checking', 'Checking what merging back would change…')}</Typography>}
        {preview.error !== null && <Alert severity="error">{describeWorktreeError(preview.error)}</Alert>}
        {preflight !== undefined && applied === undefined && <PreviewBody preflight={preflight} />}
        {apply.error !== null && <Alert severity="error">{describeWorktreeError(apply.error)}</Alert>}
        {applied !== undefined && preflight !== undefined && (
          <AppliedBody
            applied={applied}
            markers={apply.variables?.mode === 'apply_with_markers' ? markerPaths(preflight) : []}
            restored={undo.data?.restored}
            undoError={undo.error}
          />
        )}
      </DialogContent>
      <MergeActions merge={merge} onClose={onClose} />
    </Dialog>
  );
}
