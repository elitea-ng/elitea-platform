/**
 * What the turn changed on disk, after `done`: per-file status and +/- counts,
 * an expandable diff, "Undo turn" (confirmed) and a per-file "Revert".
 *
 * "Undo turn" is for the newest turn of the folder only (the host's
 * `latest`). An older turn offers "Restore folder to before this turn": its
 * confirmation lists what the host's dry run (`checkpoint_preview`) says
 * would be reverted or deleted — later turns' files included — and any
 * edits made since, and only then asks with `confirmOlder`. A per-file
 * revert of an older turn is refused by the host when the file changed
 * since (`file_changed_since`).
 *
 * With `files`, a turn the host no longer keeps (an earlier visit, a
 * restart): its changes as recorded when it ended, read-only — no undo.
 *
 * Restoring is the host's job (`checkpoint_restore`); this card only asks, and
 * then re-reads `turn_changes` so what it shows is what is on disk now.
 */
import { useState, type ReactNode } from 'react';

import { useMutation, useQuery, useQueryClient, type UseMutationResult } from '@tanstack/react-query';

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

/** The host's codes that say why (busy, expired, no checkpoint, not the newest, …) are described; anything else gets the generic `fallback`. */
const ACTIONABLE = new Set(['workspace_busy', 'turn_expired', 'no_checkpoint', 'undo_not_latest', 'file_changed_since', 'session_replaced', 'already_undone']);

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

const CHANGES_KEY = ['workspace', 'turn-changes'] as const;
const changesKey = (turnId: string) => [...CHANGES_KEY, turnId] as const;
const previewKey = (turnId: string) => ['workspace', 'restore-preview', turnId] as const;

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
  /** The newest turn of its folder whose changes stand ("Undo turn"); an older one restores the folder instead. */
  latest: boolean;
  undone: boolean;
}

/** The host's live `turn_changes`, or the recorded list as it is. */
function useChanges(ipc: WorkspaceIpc, turnId: string, recorded: readonly ChangedFile[] | undefined): ChangesSource {
  const live = useQuery({ queryKey: changesKey(turnId), queryFn: () => ipc.turnChanges(turnId), enabled: recorded === undefined });
  if (recorded !== undefined) return { isPending: false, isError: false, error: null, files: recorded, latest: false, undone: false };
  return {
    isPending: live.isPending,
    isError: live.isError,
    error: live.error,
    files: live.data?.files ?? [],
    latest: live.data?.latest ?? true,
    undone: live.data?.undone ?? false,
  };
}

interface SummaryProps {
  files: readonly ChangedFile[];
  stacked: boolean;
  /** `undefined`: read-only or undone, no undo button. */
  onUndo: (() => void) | undefined;
  /** An older turn: "Restore folder to before this turn" instead of "Undo turn". */
  older: boolean;
  undoDisabled: boolean;
  undone: boolean;
}

function Summary({ files, stacked, onUndo, older, undoDisabled, undone }: SummaryProps): React.JSX.Element {
  const added = files.reduce((sum, file) => sum + file.added, 0);
  const removed = files.reduce((sum, file) => sum + file.removed, 0);
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5 }}>
      <CardTitle hidden={stacked} />
      <Typography variant="bodySmall" sx={{ color: (theme: Theme) => theme.vars.palette.text.secondary }}>
        {t('workspace.changes.summary', '{{files}} files, +{{added}} -{{removed}}', { files: files.length, added, removed })}
      </Typography>
      {undone && <Chip size="small" label={t('workspace.changes.undone', 'Undone')} />}
      {onUndo !== undefined && (
        <Button size="small" color="warning" variant="outlined" sx={{ marginLeft: 'auto' }} disabled={undoDisabled} onClick={onUndo}>
          {older ? t('workspace.changes.restoreFolder', 'Restore folder to before this turn') : t('workspace.changes.undo', 'Undo turn')}
        </Button>
      )}
    </Box>
  );
}

/** The older turn's confirmation: what the host's dry run says a restore would revert or delete. */
function RestoreFolderPreview({ ipc, turnId }: { ipc: WorkspaceIpc; turnId: string }): React.JSX.Element {
  const preview = useQuery({ queryKey: previewKey(turnId), queryFn: () => ipc.restorePreview(turnId), gcTime: 0, staleTime: 0 });
  if (preview.isPending) return <DialogContentText>{t('workspace.changes.restoreFolderLoading', 'Working out what would change…')}</DialogContentText>;
  if (preview.isError) return <Alert severity="error">{failureText(preview.error, t('workspace.changes.restoreFolderUnknown', 'What would change could not be worked out.'))}</Alert>;
  const { restored, deleted } = preview.data;
  return (
    <>
      <DialogContentText>
        {t('workspace.changes.restoreFolderBody', 'The folder goes back to how it was before this turn. That reverts the turns after it too:')}
      </DialogContentText>
      {restored.length + deleted.length === 0 ? (
        <DialogContentText>{t('workspace.changes.restoreFolderNothing', 'No file would change.')}</DialogContentText>
      ) : (
        <Box component="ul" data-testid="restore-folder-preview" sx={{ marginY: 1, paddingLeft: 3, maxHeight: 240, overflowY: 'auto' }}>
          {restored.map((path) => (
            <Typography component="li" variant="bodySmall" key={`r-${path}`} sx={{ wordBreak: 'break-all' }}>
              {path}
            </Typography>
          ))}
          {deleted.map((path) => (
            <Typography component="li" variant="bodySmall" key={`d-${path}`} sx={{ wordBreak: 'break-all' }}>
              {t('workspace.changes.restoreFolderDeleted', '{{path}} (deleted)', { path })}
            </Typography>
          ))}
        </Box>
      )}
      <DialogContentText>{t('workspace.changes.restoreFolderEdits', '…and any edits you made since are lost.')}</DialogContentText>
    </>
  );
}

/** Why the changes could not be read, or what the last restore did. */
function CardAlerts({ changes, restore }: { changes: ChangesSource; restore: UseMutationResult<{ restored: string[] }, Error, RestoreRequest> }): React.JSX.Element {
  return (
    <>
      {changes.isError && <Alert severity="error">{failureText(changes.error, t('workspace.changes.loadFailed', 'The changes could not be read.'))}</Alert>}
      {restore.isError && <Alert severity="error">{failureText(restore.error, t('workspace.changes.restoreFailed', 'The files could not be restored.'))}</Alert>}
      {restore.isSuccess && (
        <Alert severity="success">
          {t('workspace.changes.restored', 'Restored {{n}} files.', { n: restore.data.restored.length })}
        </Alert>
      )}
    </>
  );
}

interface UndoDialogProps {
  open: boolean;
  /** An older turn: the folder goes back to before it (with the host's dry run listed). */
  older: boolean;
  ipc: WorkspaceIpc;
  turnId: string;
  onClose: () => void;
  onConfirm: () => void;
}

/** "Undo this turn?" for the newest turn, "Restore the folder to before this turn?" for an older one. */
function UndoDialog({ open, older, ipc, turnId, onClose, onConfirm }: UndoDialogProps): React.JSX.Element {
  return (
    <Dialog open={open} onClose={onClose} aria-labelledby="undo-turn-title">
      <DialogTitle id="undo-turn-title">
        {older ? t('workspace.changes.restoreFolderTitle', 'Restore the folder to before this turn?') : t('workspace.changes.undoTitle', 'Undo this turn?')}
      </DialogTitle>
      <DialogContent>
        {older ? (
          open && <RestoreFolderPreview ipc={ipc} turnId={turnId} />
        ) : (
          <DialogContentText>
            {t('workspace.changes.undoBody', 'Every file the agent changed in this turn goes back to how it was before. Later edits to those files are lost.')}
          </DialogContentText>
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t('workspace.changes.undoCancel', 'Keep changes')}</Button>
        <Button color="warning" variant="contained" onClick={onConfirm}>
          {older ? t('workspace.changes.restoreFolderConfirm', 'Restore folder') : t('workspace.changes.undoConfirm', 'Undo turn')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

interface RestoreRequest {
  path?: string;
  confirmOlder?: boolean;
}

export function ChangedFilesCard({ ipc, turnId, undoRequest = 0, variant, fileActions, files: recorded }: ChangedFilesCardProps): React.JSX.Element | null {
  const stacked = variant === 'panel';
  const readOnly = recorded !== undefined;
  const queryClient = useQueryClient();
  const [confirmingUndo, setConfirmingUndo] = useConfirmation(undoRequest);
  const changes = useChanges(ipc, turnId, recorded);
  const restore = useMutation({
    mutationFn: ({ path, confirmOlder }: RestoreRequest) => ipc.restore(turnId, path, confirmOlder === true ? { confirmOlder } : undefined),
    // Every turn's offer may change (newest, undone): all of them are read again.
    onSettled: () => queryClient.invalidateQueries({ queryKey: CHANGES_KEY }),
  });

  if (changes.isPending) return null;
  const { files, undone } = changes;
  if (!changes.isError && files.length === 0 && !restore.isSuccess) return null;
  const older = !changes.latest;
  const canRestore = !readOnly && !undone;

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
        onUndo={canRestore ? () => setConfirmingUndo(true) : undefined}
        older={older}
        undoDisabled={restore.isPending || files.length === 0}
        undone={undone}
      />
      <CardAlerts changes={changes} restore={restore} />
      <Box component="ul" sx={{ margin: 0, padding: 0 }}>
        {files.map((file) => (
          <FileRow
            key={file.path}
            file={file}
            busy={restore.isPending}
            stacked={stacked}
            fileActions={fileActions}
            onRevert={canRestore ? () => restore.mutate({ path: file.path }) : undefined}
          />
        ))}
      </Box>
      <UndoDialog
        open={confirmingUndo && canRestore}
        older={older}
        ipc={ipc}
        turnId={turnId}
        onClose={() => setConfirmingUndo(false)}
        onConfirm={() => {
          setConfirmingUndo(false);
          restore.mutate(older ? { confirmOlder: true } : {});
        }}
      />
    </Box>
  );
}
