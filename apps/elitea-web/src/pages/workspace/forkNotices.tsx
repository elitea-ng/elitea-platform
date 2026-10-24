/**
 * The notices of a fork above a thread's composer: what the fork that opened
 * this thread copied, why the last fork from here did not happen, and
 * "Restore files to this point".
 *
 * A fork never changes the folder. When its fork point is a turn the host
 * still keeps, the folder as that turn left it is the checkpoint of the next
 * later turn of the folder that changed files (`checkpoint_after`, IPC.md
 * "Thread history"). The restore is that older turn's restore: the host's dry
 * run lists what would be written back and deleted (later turns' files and
 * the person's own edits included), and only a confirmation restores
 * (`confirmOlder`). Checkpoints belong to the folder, so the source thread's
 * history is not changed; its turns after the fork point read as undone.
 */
import { useState } from 'react';

import { useMutation, useQuery } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';

import { describeWorkspaceError } from '@/features/workspace';

import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import type { SessionFork } from './useForkThread';

/** The dry run of restoring the folder to before `turnId`, as a list. */
function RestorePreviewList({ ipc, turnId }: { ipc: WorkspaceIpc; turnId: string }): React.JSX.Element {
  const preview = useQuery({ queryKey: ['workspace', 'fork-restore-preview', turnId], queryFn: () => ipc.restorePreview(turnId), gcTime: 0 });
  if (preview.isPending) return <DialogContentText>{t('workspace.fork.restoreChecking', 'Checking what would change…')}</DialogContentText>;
  if (preview.isError) return <Alert severity="error">{describeWorkspaceError(preview.error)}</Alert>;
  const { restored, deleted } = preview.data;
  if (restored.length === 0 && deleted.length === 0) {
    return <DialogContentText>{t('workspace.fork.restoreNothing', 'The files are already as they were at that point.')}</DialogContentText>;
  }
  return (
    <Box component="ul" data-testid="fork-restore-preview" sx={{ margin: 0, paddingLeft: 2.5 }}>
      {restored.map((path) => (
        <li key={`r:${path}`}>{t('workspace.fork.restoreWritten', '{{path}} is written back', { path })}</li>
      ))}
      {deleted.map((path) => (
        <li key={`d:${path}`}>{t('workspace.fork.restoreDeleted', '{{path}} is deleted', { path })}</li>
      ))}
    </Box>
  );
}

/** "Restore files to this point" for the fork point turn `turnId`; nothing when no later turn changed the folder. */
function ForkRestoreFiles({ ipc, turnId }: { ipc: WorkspaceIpc; turnId: string }): React.JSX.Element | null {
  const [confirming, setConfirming] = useState(false);
  // An expired turn (or an unknown one) offers nothing: the host no longer keeps its checkpoint.
  const point = useQuery({ queryKey: ['workspace', 'fork-restore-point', turnId], queryFn: () => ipc.restorePointAfter(turnId), retry: false });
  const restore = useMutation({ mutationFn: (next: string) => ipc.restore(next, undefined, { confirmOlder: true }) });
  const next = point.data;
  if (typeof next !== 'string') return null;
  if (restore.isSuccess) {
    return (
      <Alert severity="success" data-testid="fork-restored">
        {t('workspace.fork.restored', 'The files are back to how they were at that point.')}
      </Alert>
    );
  }
  return (
    <>
      <Alert
        severity="info"
        data-testid="fork-restore"
        action={
          <Button color="inherit" size="small" disabled={restore.isPending} onClick={() => setConfirming(true)}>
            {t('workspace.fork.restoreAction', 'Restore files')}
          </Button>
        }
      >
        {restore.isError ? describeWorkspaceError(restore.error) : t('workspace.fork.restoreOffer', 'The files can go back to how they were at the fork point.')}
      </Alert>
      <Dialog open={confirming} onClose={() => setConfirming(false)} aria-labelledby="fork-restore-title">
        <DialogTitle id="fork-restore-title">{t('workspace.fork.restoreTitle', 'Restore files to the fork point?')}</DialogTitle>
        <DialogContent>{confirming && <RestorePreviewList ipc={ipc} turnId={next} />}</DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirming(false)}>{t('workspace.changes.undoCancel', 'Keep changes')}</Button>
          <Button
            color="warning"
            variant="contained"
            onClick={() => {
              setConfirming(false);
              restore.mutate(next);
            }}
          >
            {t('workspace.changes.restoreFolderConfirm', 'Restore folder')}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}

/** What the fork that opened this thread copied (and its files offer), and why the last fork from here did not happen. */
export function ForkNotices({ fork, ipc }: { fork: SessionFork; ipc: WorkspaceIpc }): React.JSX.Element {
  return (
    <>
      {fork.notice !== null && (
        <Alert severity={fork.notice.severity} onClose={fork.dismissNotice} data-testid="fork-notice">
          {fork.notice.text}
        </Alert>
      )}
      {fork.notice?.restoreTo !== undefined && <ForkRestoreFiles ipc={ipc} turnId={fork.notice.restoreTo} />}
      {fork.error !== null && (
        <Alert severity="error" onClose={fork.clearError} data-testid="fork-error">
          {fork.error}
        </Alert>
      )}
    </>
  );
}
