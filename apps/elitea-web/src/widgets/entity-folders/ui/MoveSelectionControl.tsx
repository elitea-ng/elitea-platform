import { useState, type ReactNode } from 'react';

import FolderOpenOutlinedIcon from '@mui/icons-material/FolderOpenOutlined';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';

import { folderOfEntity, useEntityFolderMutations, useEntityFolders, type EntityFolderKind } from '@/entities/entity-folder';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { entityFolderAccess } from '../lib/access';

export interface MoveSelectionControlProps {
  readonly projectId: string | undefined;
  readonly kind: EntityFolderKind;
  /** The caller's project permissions: moving needs the list's update permission, and without it the control draws nothing. */
  readonly permissions: ReadonlySet<string>;
  /** The selected row ids, as the list shows them. */
  readonly ids: readonly string[];
  /** After the moves, with the ids the server refused, so the page keeps only those selected. */
  readonly onSettled: (result: { readonly failed: readonly string[] }) => void;
  readonly testIdPrefix: string;
}

/**
 * "Move to folder" in a list's selection section (the `extra` slot of
 * `ListSelectionSection`): the non-drag way to put every selected row into one
 * folder, or take them all out. It is the multi-select equivalent of the card's
 * Move to folder and of a drag.
 */
export function MoveSelectionControl({ projectId, kind, permissions, ids, onSettled, testIdPrefix }: MoveSelectionControlProps): ReactNode {
  const allowed = entityFolderAccess(kind, permissions).canUpdate && projectId !== undefined;
  const folders = useEntityFolders(projectId, kind).data ?? [];
  const { moveMany } = useEntityFolderMutations(projectId, kind);
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [busy, setBusy] = useState(false);
  if (!allowed) return null;

  const choose = (folderId: string | null): void => {
    setAnchor(null);
    // A row already where it is stays put (it would only be re-topped). Each move lands at the top of the folder, so the last move
    // goes on top: send them in reverse and the folder reads in the order the rows were selected.
    const pending = ids
      .map(Number)
      .filter((id) => (folderOfEntity(folders, id)?.id ?? null) !== folderId)
      .reverse();
    if (pending.length === 0) {
      onSettled({ failed: [] });
      return;
    }
    setBusy(true);
    void moveMany(pending, folderId)
      .then(({ failed }) => {
        onSettled({ failed: failed.map(String) });
      })
      .finally(() => {
        setBusy(false);
      });
  };
  const label = t('widgets.entityFolders.moveSelection', 'Move to folder');
  return (
    <>
      <BaseBtn
        variant="secondary"
        startIcon={<FolderOpenOutlinedIcon />}
        disabled={busy}
        aria-haspopup="menu"
        aria-expanded={anchor !== null}
        onClick={(event) => {
          setAnchor(event.currentTarget);
        }}
        data-testid={`${testIdPrefix}-selection-move`}
      >
        {label}
      </BaseBtn>
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={() => {
          setAnchor(null);
        }}
        slotProps={{ list: { 'aria-label': label } }}
      >
        {folders.length === 0 && <MenuItem disabled>{t('widgets.entityFolders.noneYet', 'No folders yet. Create one in the Filters panel.')}</MenuItem>}
        {folders.map((folder) => (
          <MenuItem
            key={folder.id}
            data-testid="move-selection-option"
            onClick={() => {
              choose(folder.id);
            }}
          >
            {folder.name}
          </MenuItem>
        ))}
        <MenuItem
          data-testid="move-selection-root-option"
          onClick={() => {
            choose(null);
          }}
        >
          {t('widgets.entityFolders.removeFromFolder', 'Remove from folder')}
        </MenuItem>
      </Menu>
    </>
  );
}
