/**
 * A chat folder's hover "⋯": Rename, Pin / Unpin and Delete…, each its
 * owner's with the project's folder right. Delete asks first.
 */
import { useState } from 'react';

import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { RowMenu } from './RowMenu';

export interface FolderMenuProps {
  readonly name: string;
  readonly pinned: boolean;
  readonly canUpdate: boolean;
  readonly canDelete: boolean;
  readonly onRename: () => void;
  readonly onPin: (pin: boolean) => void;
  readonly onDelete: () => void;
}

export function FolderMenu({ name, pinned, canUpdate, canDelete, onRename, onPin, onDelete }: FolderMenuProps): React.JSX.Element {
  const [deleting, setDeleting] = useState(false);
  return (
    <>
      <RowMenu
        testId="shell-chats-folder-more"
        label={t('desktop.chats.folderMore', 'More for folder {{name}}', { name })}
        items={[
          { id: 'rename', label: t('desktop.chats.rename', 'Rename'), enabled: canUpdate, run: onRename },
          { id: 'pin', label: pinned ? t('desktop.chats.unpin', 'Unpin') : t('desktop.chats.pin', 'Pin'), enabled: canUpdate, run: () => onPin(!pinned) },
          { separator: true },
          { id: 'delete', label: t('desktop.chats.delete', 'Delete…'), enabled: canDelete, run: () => setDeleting(true) },
        ]}
      />
      <DeleteEntityModal
        open={deleting}
        name={name}
        onClose={() => setDeleting(false)}
        onConfirm={() => {
          setDeleting(false);
          onDelete();
        }}
        copy={{ title: t('desktop.chats.deleteFolderTitle', 'Delete folder'), textContent: t('desktop.chats.deleteFolderText', "This can't be undone. Delete ") }}
      />
    </>
  );
}
