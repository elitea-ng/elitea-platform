/**
 * The two confirmation dialogs of the artifacts page, split out of
 * `Artifacts.tsx` to keep it under the 400-line budget.
 */
import type { ReactNode } from 'react';

import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';

import { t } from '@/shared/i18n';

export function DeleteArtifactsDialog(props: {
  readonly open: boolean;
  readonly pending: boolean;
  readonly onClose: () => void;
  readonly onConfirm: () => void;
}): ReactNode {
  return (
    <Dialog open={props.open} onClose={props.onClose}>
      <DialogTitle>{t('artifacts.page.deleteTitle', 'Delete selected artifacts?')}</DialogTitle>
      <DialogContent>{t('artifacts.page.deleteDescription', 'This action cannot be undone.')}</DialogContent>
      <DialogActions>
        <Button onClick={props.onClose}>{t('common.cancel', 'Cancel')}</Button>
        <Button color="error" variant="contained" disabled={props.pending} onClick={props.onConfirm}>
          {t('common.delete', 'Delete')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

export function DiscardChangesDialog(props: {
  readonly open: boolean;
  readonly onKeepEditing: () => void;
  readonly onDiscard: () => void;
}): ReactNode {
  return (
    <Dialog open={props.open} onClose={props.onKeepEditing}>
      <DialogTitle>{t('artifacts.page.unsavedTitle', 'Discard unsaved changes?')}</DialogTitle>
      <DialogContent>{t('artifacts.page.unsavedDescription', 'Your current file edits will be lost.')}</DialogContent>
      <DialogActions>
        <Button onClick={props.onKeepEditing}>{t('artifacts.preview.keepEditing', 'Keep editing')}</Button>
        <Button color="warning" onClick={props.onDiscard}>
          {t('common.discard', 'Discard')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
