/**
 * Erase the cloud copy of a thread: the files the cloud holds, and what the
 * cloud's turns changed in them. It asks first, because those changes are
 * gone for good; your folder on this computer is never touched.
 */
import { useMutation } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';

import type { HandoffIpc } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

import { describeHandoffError } from '../model/describeHandoffError';

export interface EraseCloudCopyDialogProps {
  open: boolean;
  onClose: () => void;
  ipc: HandoffIpc;
  conversationUuid: string;
  /** The thread's project, for a thread this computer holds no record of. */
  projectId?: number;
  onErased: () => void;
}

export function EraseCloudCopyDialog({ open, onClose, ipc, conversationUuid, projectId, onErased }: EraseCloudCopyDialogProps): React.JSX.Element {
  const erase = useMutation({
    mutationFn: () => ipc.erase(conversationUuid, projectId),
    onSuccess: onErased,
  });
  return (
    <Dialog open={open} onClose={erase.isPending ? undefined : onClose} aria-labelledby="handoff-erase-title" fullWidth maxWidth="xs">
      <DialogTitle id="handoff-erase-title">{t('workspace.handoff.eraseTitle', 'Erase the cloud copy?')}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <Typography variant="bodyMedium">
          {t(
            'workspace.handoff.eraseBody',
            'The files in the cloud, and every change the cloud made to them, are deleted. A turn that is running is stopped. Your folder on this computer is kept as it is, and the thread goes back to running here.',
          )}
        </Typography>
        {erase.error !== null && <Alert severity="error">{describeHandoffError(erase.error)}</Alert>}
      </DialogContent>
      <DialogActions>
        <Button disabled={erase.isPending} onClick={onClose}>
          {t('workspace.handoff.keep', 'Keep it')}
        </Button>
        <Button variant="contained" color="error" disabled={erase.isPending} onClick={() => erase.mutate()}>
          {t('workspace.handoff.erase', 'Erase cloud copy')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
