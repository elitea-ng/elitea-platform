/**
 * The questions Archive and Delete ask about a Local work thread
 * (`useThreadLifecycle`), in the native-looking dialog of the sidebar
 * (`MoveFolderDialog.tsx`): Delete always asks; Archive only for a thread
 * with a task worktree. A thread with a worktree shows "Also remove its
 * worktree", checked by default, and says its branch is kept when it has
 * commits. A worktree the host refused to remove for uncommitted work asks
 * "Remove anyway" or "Keep the worktree".
 */
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import type { LifecycleKind, ThreadLifecycle } from '../model/useThreadLifecycle';

const TITLE_ID = 'thread-lifecycle-title';

function DirtyWorktree({ lifecycle }: { readonly lifecycle: ThreadLifecycle }): React.JSX.Element {
  const branch = lifecycle.worktree?.branch ?? '';
  return (
    <>
      <DialogTitle id={TITLE_ID}>{t('desktop.shell.worktreeDirtyTitle', 'The worktree has uncommitted work')}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <DialogContentText>
          {t('desktop.shell.worktreeDirtyBody', '{{branch}} has changes that were never committed. Removing the worktree deletes them for good; the main folder is not touched.', { branch })}
        </DialogContentText>
        {lifecycle.error !== null && <Alert severity="error">{lifecycle.error}</Alert>}
      </DialogContent>
      <DialogActions>
        <Button disabled={lifecycle.busy} onClick={lifecycle.close}>
          {t('desktop.shell.keepWorktree', 'Keep the worktree')}
        </Button>
        <Button variant="contained" color="error" disabled={lifecycle.busy} onClick={lifecycle.forceRemove} data-testid="thread-lifecycle-force">
          {t('workspace.worktree.removeAnyway', 'Remove anyway')}
        </Button>
      </DialogActions>
    </>
  );
}

function Confirm({ lifecycle, kind, title }: { readonly lifecycle: ThreadLifecycle; readonly kind: LifecycleKind; readonly title: string }): React.JSX.Element {
  const [withWorktree, setWithWorktree] = useState(true);
  const worktree = lifecycle.worktree;
  const archive = kind === 'archive';
  return (
    <>
      <DialogTitle id={TITLE_ID}>
        {archive ? t('desktop.shell.archiveThreadTitle', 'Archive “{{name}}”?', { name: title }) : t('desktop.shell.deleteThreadTitle', 'Delete “{{name}}”?', { name: title })}
      </DialogTitle>
      <DialogContent>
        <DialogContentText>
          {archive
            ? t('desktop.shell.archiveThreadBody', "The thread leaves Local work and moves to the project's Archived chats. Unarchive it there to bring it back.")
            : t('desktop.shell.deleteThreadBody', 'The conversation is deleted, and its history on this computer. This can’t be undone.')}
        </DialogContentText>
        {worktree !== undefined && (
          <>
            <FormControlLabel
              sx={{ marginTop: 1 }}
              control={<BaseCheckbox size="small" checked={withWorktree} onChange={(event) => setWithWorktree(event.target.checked)} />}
              label={t('desktop.shell.alsoRemoveWorktree', 'Also remove its worktree')}
            />
            <Typography variant="bodySmall" component="p" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
              {t('desktop.shell.worktreeBranchKept', 'The branch {{branch}} is kept when it has commits.', { branch: worktree.branch })}
            </Typography>
          </>
        )}
      </DialogContent>
      <DialogActions>
        <Button disabled={lifecycle.busy} onClick={lifecycle.close}>
          {t('desktop.shell.moveFolderCancel', 'Cancel')}
        </Button>
        <Button
          variant="contained"
          color={archive ? 'primary' : 'error'}
          disabled={lifecycle.busy}
          onClick={() => lifecycle.confirm(worktree !== undefined && withWorktree)}
          data-testid="thread-lifecycle-confirm"
        >
          {archive ? t('desktop.chats.archive', 'Archive') : t('desktop.shell.deleteThread', 'Delete')}
        </Button>
      </DialogActions>
    </>
  );
}

export function ThreadLifecycleDialog({ lifecycle, title }: { readonly lifecycle: ThreadLifecycle; readonly title: string }): React.JSX.Element | null {
  const { ask } = lifecycle;
  if (ask === null) return null;
  return (
    <Dialog open onClose={lifecycle.busy ? undefined : lifecycle.close} maxWidth="xs" fullWidth aria-labelledby={TITLE_ID} data-testid="thread-lifecycle-dialog">
      {ask.kind === 'dirty' ? <DirtyWorktree lifecycle={lifecycle} /> : <Confirm key={ask.kind} lifecycle={lifecycle} kind={ask.kind} title={title} />}
    </Dialog>
  );
}
