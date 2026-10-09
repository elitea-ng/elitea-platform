/**
 * What the turn changed on disk, after `done`: per-file status and +/- counts,
 * an expandable diff, "Undo turn" (confirmed) and a per-file "Revert".
 *
 * Restoring is the host's job (`checkpoint_restore`); this card only asks, and
 * then re-reads `turn_changes` so what it shows is what is on disk now.
 */
import { useState } from 'react';

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

import type { ChangedFile, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { DiffView } from '@/shared/ui/DiffView';

import { parseUnifiedDiff } from '../model/unifiedDiff';

export interface ChangedFilesCardProps {
  ipc: WorkspaceIpc;
  turnId: string;
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

function FileRow({ file, busy, onRevert }: { file: ChangedFile; busy: boolean; onRevert: () => void }): React.JSX.Element {
  const [open, setOpen] = useState(false);
  const diffId = `diff-${file.path}`;
  return (
    <Box component="li" data-testid="changed-file" sx={{ listStyle: 'none', paddingY: 0.5 }}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
        <Chip size="small" variant="outlined" label={statusLabel(file.status)} />
        <Typography variant="labelMedium" sx={{ wordBreak: 'break-all' }}>
          {file.path}
        </Typography>
        <Typography variant="bodySmall" sx={{ color: (theme: Theme) => theme.vars.palette.text.secondary }}>
          {`+${String(file.added)} -${String(file.removed)}`}
        </Typography>
        <Box sx={{ marginLeft: 'auto', display: 'flex', gap: 1 }}>
          {file.diff !== '' && (
            <Button size="small" aria-expanded={open} aria-controls={diffId} onClick={() => setOpen((value) => !value)}>
              {open ? t('workspace.changes.hideDiff', 'Hide diff') : t('workspace.changes.showDiff', 'Show diff')}
            </Button>
          )}
          <Button size="small" color="warning" disabled={busy} onClick={onRevert} aria-label={t('workspace.changes.revertFile', 'Revert {{path}}', { path: file.path })}>
            {t('workspace.changes.revert', 'Revert')}
          </Button>
        </Box>
      </Box>
      {file.diff !== '' && (
        <Collapse in={open} unmountOnExit>
          <Box id={diffId} sx={{ paddingTop: 1 }}>
            <DiffView parts={parseUnifiedDiff(file.diff)} />
          </Box>
        </Collapse>
      )}
    </Box>
  );
}

export function ChangedFilesCard({ ipc, turnId }: ChangedFilesCardProps): React.JSX.Element | null {
  const queryClient = useQueryClient();
  const [confirmingUndo, setConfirmingUndo] = useState(false);
  const changes = useQuery({ queryKey: changesKey(turnId), queryFn: () => ipc.turnChanges(turnId) });
  const restore = useMutation({
    mutationFn: (path: string | undefined) => ipc.restore(turnId, path),
    onSettled: () => queryClient.invalidateQueries({ queryKey: changesKey(turnId) }),
  });

  if (changes.isPending) return null;
  const files = changes.data?.files ?? [];
  if (!changes.isError && files.length === 0 && !restore.isSuccess) return null;

  const added = files.reduce((sum, file) => sum + file.added, 0);
  const removed = files.reduce((sum, file) => sum + file.removed, 0);

  return (
    <Box
      data-testid="changed-files-card"
      component="section"
      aria-label={t('workspace.changes.title', 'Changed files')}
      sx={(theme: Theme) => ({
        border: `1px solid ${theme.vars.palette.divider}`,
        borderRadius: theme.vars.shape.radiusMd,
        padding: 1.5,
      })}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5 }}>
        <Typography variant="headingSmall">{t('workspace.changes.title', 'Changed files')}</Typography>
        <Typography variant="bodySmall" sx={{ color: (theme: Theme) => theme.vars.palette.text.secondary }}>
          {t('workspace.changes.summary', '{{files}} files, +{{added}} -{{removed}}', { files: files.length, added, removed })}
        </Typography>
        <Button
          size="small"
          color="warning"
          variant="outlined"
          sx={{ marginLeft: 'auto' }}
          disabled={restore.isPending || files.length === 0}
          onClick={() => setConfirmingUndo(true)}
        >
          {t('workspace.changes.undo', 'Undo turn')}
        </Button>
      </Box>
      {changes.isError && <Alert severity="error">{t('workspace.changes.loadFailed', 'The changes could not be read.')}</Alert>}
      {restore.isError && <Alert severity="error">{t('workspace.changes.restoreFailed', 'The files could not be restored.')}</Alert>}
      {restore.isSuccess && (
        <Alert severity="success">
          {t('workspace.changes.restored', 'Restored {{n}} files.', { n: restore.data.restored.length })}
        </Alert>
      )}
      <Box component="ul" sx={{ margin: 0, padding: 0 }}>
        {files.map((file) => (
          <FileRow key={file.path} file={file} busy={restore.isPending} onRevert={() => restore.mutate(file.path)} />
        ))}
      </Box>
      <Dialog open={confirmingUndo} onClose={() => setConfirmingUndo(false)} aria-labelledby="undo-turn-title">
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
