/**
 * The confirmation a remove needs when the host refused it with
 * `worktree_dirty`: the task has work that was never committed, and removing
 * it anyway (`force`) deletes that work for good.
 */
import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';

import type { WorktreeEntry } from '@/shared/desktop/worktreeIpc';
import { t } from '@/shared/i18n';

export interface RemoveWorktreeDialogProps {
  entry: WorktreeEntry;
  busy: boolean;
  error: string | null;
  onConfirm: () => void;
  onClose: () => void;
}

export function RemoveWorktreeDialog({ entry, busy, error, onConfirm, onClose }: RemoveWorktreeDialogProps): React.JSX.Element {
  return (
    <Dialog open onClose={busy ? undefined : onClose} aria-labelledby="worktree-remove-title" fullWidth maxWidth="xs">
      <DialogTitle id="worktree-remove-title">{t('workspace.worktree.removeTitle', 'Remove {{branch}}?', { branch: entry.branch })}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <Typography variant="bodyMedium">
          {t('workspace.worktree.removeDirty', 'This task has changes that were never committed. Removing it deletes them for good; the main folder is not touched.')}
        </Typography>
        {error !== null && <Alert severity="error">{error}</Alert>}
      </DialogContent>
      <DialogActions>
        <Button disabled={busy} onClick={onClose}>
          {t('workspace.worktree.keep', 'Keep it')}
        </Button>
        <Button variant="contained" color="error" disabled={busy} onClick={onConfirm}>
          {t('workspace.worktree.removeAnyway', 'Remove anyway')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
