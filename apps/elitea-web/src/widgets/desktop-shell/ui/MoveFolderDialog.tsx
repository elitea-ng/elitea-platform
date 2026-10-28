/**
 * Confirms moving a folder to another project. A folder belongs to the
 * project the sidebar shows it under: its agent runs use that project's
 * models, credentials and budget, and it leaves the other project's sidebar.
 * A folder with no project yet joins one without asking.
 */
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';

import { t } from '@/shared/i18n';

export interface PendingMove {
  readonly folder: string;
  readonly from: string;
  readonly to: string;
  readonly confirm: () => void;
}

export function MoveFolderDialog({ move, onClose }: { readonly move: PendingMove | null; readonly onClose: () => void }): React.JSX.Element {
  return (
    <Dialog open={move !== null} onClose={onClose} maxWidth="xs" fullWidth aria-labelledby="move-folder-title" data-testid="move-folder-dialog">
      <DialogTitle id="move-folder-title">
        {t('desktop.shell.moveFolderTitle', 'Move “{{folder}}” to {{project}}?', { folder: move?.folder ?? '', project: move?.to ?? '' })}
      </DialogTitle>
      <DialogContent>
        <DialogContentText>
          {t(
            'desktop.shell.moveFolderBody',
            'The folder leaves {{from}} and shows under {{to}}. Its next threads use {{to}}’s models, credentials and budget; the files and past threads stay on this computer.',
            { from: move?.from ?? '', to: move?.to ?? '' },
          )}
        </DialogContentText>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t('desktop.shell.moveFolderCancel', 'Cancel')}</Button>
        <Button
          variant="contained"
          onClick={() => {
            move?.confirm();
            onClose();
          }}
        >
          {t('desktop.shell.moveFolderConfirm', 'Move')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
