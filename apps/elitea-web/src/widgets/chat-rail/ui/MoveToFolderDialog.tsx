/**
 * "Move to folder…" for one conversation of the sidebar's Chats list: the
 * project's chat folders, and "No folder" to take the conversation out of the
 * one that holds it. The choice is made at once (no confirm button), as in a
 * menu; the move itself is `useConversationRail`'s (the web list's hook).
 */
import Dialog from '@mui/material/Dialog';
import DialogTitle from '@mui/material/DialogTitle';
import List from '@mui/material/List';
import ListItemButton from '@mui/material/ListItemButton';
import ListItemText from '@mui/material/ListItemText';

import { t } from '@/shared/i18n';

export interface FolderChoice {
  readonly id: string;
  readonly name: string;
}

export interface MoveToFolderDialogProps {
  readonly open: boolean;
  readonly name: string;
  readonly folders: readonly FolderChoice[];
  /** The folder that holds the conversation now; `null` when none does. */
  readonly currentFolderId: string | null;
  readonly onClose: () => void;
  readonly onMove: (folderId: string | null) => void;
}

export function MoveToFolderDialog({ open, name, folders, currentFolderId, onClose, onMove }: MoveToFolderDialogProps): React.JSX.Element {
  const choose = (folderId: string | null): void => {
    onClose();
    if (folderId !== currentFolderId) onMove(folderId);
  };
  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="xs">
      <DialogTitle>{t('desktop.chats.moveTitle', 'Move “{{name}}” to', { name })}</DialogTitle>
      <List dense data-testid="shell-move-to-folder" sx={{ paddingTop: 0 }}>
        {folders.map((folder) => (
          <ListItemButton key={folder.id} selected={folder.id === currentFolderId} onClick={() => choose(folder.id)}>
            <ListItemText primary={folder.name} />
          </ListItemButton>
        ))}
        <ListItemButton selected={currentFolderId === null} onClick={() => choose(null)}>
          <ListItemText primary={t('desktop.chats.noFolder', 'No folder')} />
        </ListItemButton>
      </List>
    </Dialog>
  );
}
