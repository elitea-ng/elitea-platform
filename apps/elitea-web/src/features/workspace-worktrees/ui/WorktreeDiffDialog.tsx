/**
 * What a parallel task changed: its branch's diff against where it started
 * (`worktree_diff`), committed and uncommitted work together. Read-only.
 */
import { useQuery } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';

import type { WorktreeEntry, WorktreeIpc } from '@/shared/desktop/worktreeIpc';
import { t } from '@/shared/i18n';
import { DiffView } from '@/shared/ui/DiffView';

import { describeWorktreeError } from '../model/describeWorktreeError';
import { diffParts } from '../model/diffParts';

export interface WorktreeDiffDialogProps {
  ipc: WorktreeIpc;
  workspaceId: string;
  entry: WorktreeEntry;
  onClose: () => void;
}

export function WorktreeDiffDialog({ ipc, workspaceId, entry, onClose }: WorktreeDiffDialogProps): React.JSX.Element {
  const diff = useQuery({ queryKey: ['worktree', 'diff', workspaceId, entry.thread_uuid], queryFn: () => ipc.diff(workspaceId, entry.thread_uuid), staleTime: 0 });
  return (
    <Dialog open onClose={onClose} aria-labelledby="worktree-diff-title" fullWidth maxWidth="md">
      <DialogTitle id="worktree-diff-title">{t('workspace.worktree.diffTitle', 'Changes on {{branch}}', { branch: entry.branch })}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        {diff.isPending && <Typography variant="bodyMedium">{t('workspace.worktree.loadingDiff', 'Reading the changes…')}</Typography>}
        {diff.error !== null && <Alert severity="error">{describeWorktreeError(diff.error)}</Alert>}
        {diff.data?.truncated === true && <Alert severity="info">{t('workspace.worktree.diffTruncated', 'The diff is too long to show in full; the first part is shown.')}</Alert>}
        {diff.data !== undefined && diff.data.diff.trim() === '' && (
          <Typography variant="bodyMedium">{t('workspace.worktree.noChanges', 'Nothing has changed on this branch yet.')}</Typography>
        )}
        {diff.data !== undefined && diff.data.diff.trim() !== '' && <DiffView data-testid="worktree-diff" parts={diffParts(diff.data.diff)} />}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t('workspace.worktree.close', 'Close')}</Button>
      </DialogActions>
    </Dialog>
  );
}
