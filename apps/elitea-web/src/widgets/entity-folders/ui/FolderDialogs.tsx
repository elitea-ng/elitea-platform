import type { ReactNode } from 'react';

import { useEntityFolderMutations, type EntityFolder, type EntityFolderKind } from '@/entities/entity-folder';
import { serverErrorOf } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { useRailFolderSelection } from '@/shared/ui/EntityRail';

import { FolderNameDialog } from './FolderNameDialog';

/** `'create'` for the new-folder dialog, else the folder being renamed. */
export type NameTarget = 'create' | EntityFolder;

interface DialogHostProps {
  readonly projectId: string | undefined;
  readonly kind: EntityFolderKind;
  readonly onClose: () => void;
}

/** The create / rename dialog. The server's refusal (a duplicate name) stays inside it; success closes it. */
export function NameDialogHost({
  projectId,
  kind,
  target,
  onClose,
}: DialogHostProps & { readonly target: NameTarget }): ReactNode {
  const { create, rename } = useEntityFolderMutations(projectId, kind);
  const creating = target === 'create';
  const mutation = creating ? create : rename;
  const submit = (name: string): void => {
    const done = { onSuccess: onClose };
    if (creating) create.mutate(name, done);
    else rename.mutate({ folderId: target.id, name }, done);
  };
  return (
    <FolderNameDialog
      key={creating ? 'create' : target.id}
      open
      initialName={creating ? '' : target.name}
      title={
        creating
          ? t('widgets.entityFolders.createTitle', 'New folder')
          : t('widgets.entityFolders.renameTitle', 'Rename folder')
      }
      confirmText={
        creating
          ? t('widgets.entityFolders.createConfirm', 'Create')
          : t('widgets.entityFolders.renameConfirm', 'Rename')
      }
      errorMessage={
        mutation.isError
          ? (serverErrorOf(mutation.error) ?? t('widgets.entityFolders.saveFailed', 'The folder could not be saved.'))
          : undefined
      }
      busy={mutation.isPending}
      onClose={onClose}
      onSubmit={submit}
    />
  );
}

/** The delete confirmation. Deleting returns the folder's entities to the root and deletes none of them. */
export function DeleteDialogHost({
  projectId,
  kind,
  folder,
  onClose,
}: DialogHostProps & { readonly folder: EntityFolder }): ReactNode {
  const { remove } = useEntityFolderMutations(projectId, kind);
  const { selectedFolderId, selectFolder } = useRailFolderSelection();
  return (
    <DeleteEntityModal
      open
      name={folder.name}
      data-testid="entity-folder-delete-dialog"
      confirming={remove.isPending}
      {...(remove.isError
        ? { errorMessage: serverErrorOf(remove.error) ?? t('widgets.entityFolders.deleteFailed', 'The folder could not be deleted.') }
        : {})}
      copy={{
        title: t('widgets.entityFolders.deleteTitle', 'Delete folder'),
        textContent: t(
          'widgets.entityFolders.deleteText',
          'Its items go back to the list; nothing else is deleted. Delete folder ',
        ),
      }}
      onClose={onClose}
      onConfirm={() => {
        remove.mutate(folder.id, {
          onSuccess: () => {
            if (folder.id === selectedFolderId) selectFolder(undefined);
            onClose();
          },
        });
      }}
    />
  );
}
