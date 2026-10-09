/**
 * What the turn changed on disk, after `done`: per-file status and +/- counts,
 * an expandable diff, "Undo turn" (confirmed) and a per-file "Revert".
 *
 * With `files`, a turn the host no longer keeps (an earlier visit, a
 * restart): its changes as recorded when it ended, read-only — no undo.
 *
 * Restoring is the host's job (`checkpoint_restore`); this card only asks, and
 * then re-reads `turn_changes` so what it shows is what is on disk now.
 */
import { useState, type ReactNode } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Collapse from '@mui/material/Collapse';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { toWorkspaceIpcError, type ChangedFile, type WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { DiffView } from '@/shared/ui/DiffView';

import { describeWorkspaceError } from '../model/describeWorkspaceError';
import { parseUnifiedDiff } from '../model/unifiedDiff';

/** The host's codes that say why (busy, expired, no checkpoint) are described; anything else gets the generic `fallback`. */
const ACTIONABLE = new Set(['workspace_busy', 'turn_expired', 'no_checkpoint']);

function failureText(error: unknown, fallback: string): string {
  return ACTIONABLE.has(toWorkspaceIpcError(error).code) ? describeWorkspaceError(error) : fallback;
}

export interface ChangedFilesCardProps {
  ipc: WorkspaceIpc;
  turnId: string;
  /** Bumped to ask for the "Undo turn" confirmation from outside (the composer's `/undo`). */
  undoRequest?: number;
  /** `panel`: the desktop's changes side panel — no frame, rows stacked for a narrow column. */
  variant?: 'card' | 'panel';
  /** Extra per-file actions (the desktop's "Reveal in Finder" / "Open"). */
  fileActions?: ((file: ChangedFile) => ReactNode) | undefined;
  /** The turn's changes as recorded (`thread_history`): shown read-only, `turn_changes` is not asked. */
  files?: readonly ChangedFile[] | undefined;
}

const changesKey = (turnId: string) => ['workspace', 'turn-changes', turnId] as const;

function statusLabel(status: ChangedFile['status']): string {
  switch (status) {
    case 'added':
      return t('workspace.changes.added', 'Added');
    case 'modified':
      return t('workspace.changes.modified', 'Modified');
    case 'deleted':
      return t('workspace.changes.deleted', 'Deleted');
    case 'renamed':
      return t('workspace.changes.renamed', 'Renamed');
  }
}

interface FileRowProps {
  file: ChangedFile;
  busy: boolean;
  /** `undefined`: read-only (a recorded turn). */
  onRevert: (() => void) | undefined;
  stacked: boolean;
  fileActions: ((file: ChangedFile) => ReactNode) | undefined;
}

/** The card's heading; the side panel has its own. */
function CardTitle({ hidden }: { hidden: boolean }): React.JSX.Element | null {
  return hidden ? null : <Typography variant="headingSmall">{t('workspace.changes.title', 'Changed files')}</Typography>;
}

/** A frame (card) or none (the side panel stacks rows on its own). */
function frameSx(stacked: boolean) {
  return (theme: Theme) =>
    stacked
      ? { display: 'flex', flexDirection: 'column', gap: 1 }
      : { border: `1px solid ${theme.vars.palette.divider}`, borderRadius: theme.vars.shape.radiusMd, padding: 1.5 };
}

function FileRow({ file, busy, onRevert, stacked, fileActions }: FileRowProps): React.JSX.Element {
  const actions = fileActions === undefined ? null : fileActions(file);
  const [open, setOpen] = useState(false);
  const diffId = `diff-${file.path}`;
  const buttons = (
    <Box sx={{ marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: 0.5 }}>
      {actions}
      {file.diff !== '' && (
        <Button size="small" aria-expanded={open} aria-controls={diffId} onClick={() => setOpen((value) => !value)}>
          {open ? t('workspace.changes.hideDiff', 'Hide diff') : t('workspace.changes.showDiff', 'Show diff')}
        </Button>
      )}
      {onRevert !== undefined && (
        <Button size="small" color="warning" disabled={busy} onClick={onRevert} aria-label={t('workspace.changes.revertFile', 'Revert {{path}}', { path: file.path })}>
          {t('workspace.changes.revert', 'Revert')}
        </Button>
      )}
    </Box>
  );
  const counts = (
    <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, flexShrink: 0 })}>
      {`+${String(file.added)} -${String(file.removed)}`}
    </Typography>
  );
  return (
    <Box
      component="li"
      data-testid="changed-file"
      sx={(theme: Theme) => ({ listStyle: 'none', paddingY: 0.5, borderBottom: stacked ? `1px solid ${theme.vars.palette.divider}` : undefined })}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
        <Chip size="small" variant="outlined" label={statusLabel(file.status)} />
        <Typography variant="labelMedium" sx={{ wordBreak: 'break-all', flex: stacked ? 1 : undefined, minWidth: 0 }}>
          {file.path}
        </Typography>
        {!stacked && counts}
        {!stacked && buttons}
      </Box>
      {stacked && (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
          {counts}
          {buttons}
        </Box>
      )}
      {file.diff !== '' && (
        <Collapse in={open} unmountOnExit>
          <Box id={diffId} sx={{ paddingTop: 1, overflowX: 'auto' }}>
            <DiffView parts={parseUnifiedDiff(file.diff)} />
          </Box>
        </Collapse>
      )}
    </Box>
  );
}

/**
 * The undo confirmation's open state; a new `request` value opens it once
 * (adjusted during render, not in an effect).
 */
function useConfirmation(request: number): [boolean, (open: boolean) => void] {
  const [open, setOpen] = useState(false);
  const [seen, setSeen] = useState(request);
  if (request !== seen) {
    setSeen(request);
    setOpen(true);
  }
  return [open, setOpen];
}

interface ChangesSource {
  isPending: boolean;
  isError: boolean;
  error: unknown;
  files: readonly ChangedFile[];
}

/** The host's live `turn_changes`, or the recorded list as it is. */
function useChanges(ipc: WorkspaceIpc, turnId: string, recorded: readonly ChangedFile[] | undefined): ChangesSource {
  const live = useQuery({ queryKey: changesKey(turnId), queryFn: () => ipc.turnChanges(turnId), enabled: recorded === undefined });
  if (recorded !== undefined) return { isPending: false, isError: false, error: null, files: recorded };
  return { isPending: live.isPending, isError: live.isError, error: live.error, files: live.data?.files ?? [] };
}

interface SummaryProps {
  files: readonly ChangedFile[];
  stacked: boolean;
  /** `undefined`: read-only, no "Undo turn". */
  onUndo: (() => void) | undefined;
  undoDisabled: boolean;
}

function Summary({ files, stacked, onUndo, undoDisabled }: SummaryProps): React.JSX.Element {
  const added = files.reduce((sum, file) => sum + file.added, 0);
  const removed = files.reduce((sum, file) => sum + file.removed, 0);
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5 }}>
      <CardTitle hidden={stacked} />
      <Typography variant="bodySmall" sx={{ color: (theme: Theme) => theme.vars.palette.text.secondary }}>
        {t('workspace.changes.summary', '{{files}} files, +{{added}} -{{removed}}', { files: files.length, added, removed })}
      </Typography>
      {onUndo !== undefined && (
        <Button size="small" color="warning" variant="outlined" sx={{ marginLeft: 'auto' }} disabled={undoDisabled} onClick={onUndo}>
          {t('workspace.changes.undo', 'Undo turn')}
        </Button>
      )}
    </Box>
  );
}

export function ChangedFilesCard({ ipc, turnId, undoRequest = 0, variant, fileActions, files: recorded }: ChangedFilesCardProps): React.JSX.Element | null {
  const stacked = variant === 'panel';
  const readOnly = recorded !== undefined;
  const queryClient = useQueryClient();
  const [confirmingUndo, setConfirmingUndo] = useConfirmation(undoRequest);
  const changes = useChanges(ipc, turnId, recorded);
  const restore = useMutation({
    mutationFn: (path: string | undefined) => ipc.restore(turnId, path),
    onSettled: () => queryClient.invalidateQueries({ queryKey: changesKey(turnId) }),
  });

  if (changes.isPending) return null;
  const { files } = changes;
  if (!changes.isError && files.length === 0 && !restore.isSuccess) return null;

  return (
    <Box
      data-testid="changed-files-card"
      component="section"
      aria-label={t('workspace.changes.title', 'Changed files')}
      sx={frameSx(stacked)}
    >
      <Summary
        files={files}
        stacked={stacked}
        onUndo={readOnly ? undefined : () => setConfirmingUndo(true)}
        undoDisabled={restore.isPending || files.length === 0}
      />
      {changes.isError && <Alert severity="error">{failureText(changes.error, t('workspace.changes.loadFailed', 'The changes could not be read.'))}</Alert>}
      {restore.isError && <Alert severity="error">{failureText(restore.error, t('workspace.changes.restoreFailed', 'The files could not be restored.'))}</Alert>}
      {restore.isSuccess && (
        <Alert severity="success">
          {t('workspace.changes.restored', 'Restored {{n}} files.', { n: restore.data.restored.length })}
        </Alert>
      )}
      <Box component="ul" sx={{ margin: 0, padding: 0 }}>
        {files.map((file) => (
          <FileRow
            key={file.path}
            file={file}
            busy={restore.isPending}
            stacked={stacked}
            fileActions={fileActions}
            onRevert={readOnly ? undefined : () => restore.mutate(file.path)}
          />
        ))}
      </Box>
      <Dialog open={confirmingUndo && !readOnly} onClose={() => setConfirmingUndo(false)} aria-labelledby="undo-turn-title">
        <DialogTitle id="undo-turn-title">{t('workspace.changes.undoTitle', 'Undo this turn?')}</DialogTitle>
        <DialogContent>
          <DialogContentText>
            {t('workspace.changes.undoBody', 'Every file the agent changed in this turn goes back to how it was before. Later edits to those files are lost.')}
          </DialogContentText>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirmingUndo(false)}>
            {t('workspace.changes.undoCancel', 'Keep changes')}
          </Button>
          <Button
            color="warning"
            variant="contained"
            onClick={() => {
              setConfirmingUndo(false);
              restore.mutate(undefined);
            }}
          >
            {t('workspace.changes.undoConfirm', 'Undo turn')}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}
