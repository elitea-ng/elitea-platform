import { useState, type ReactNode } from 'react';

import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import Divider from '@mui/material/Divider';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';

import { t } from '@/shared/i18n';

export interface FolderRowMenuProps {
  readonly name: string;
  readonly canMoveUp: boolean;
  readonly canMoveDown: boolean;
  readonly onRename: () => void;
  readonly onMove: (direction: 'up' | 'down') => void;
  readonly onDelete: () => void;
}

/** One folder's "⋯": Rename, Move up, Move down, Delete. Delete never deletes the entities inside. */
export function FolderRowMenu({
  name,
  canMoveUp,
  canMoveDown,
  onRename,
  onMove,
  onDelete,
}: FolderRowMenuProps): ReactNode {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const run =
    (action: () => void): (() => void) =>
    () => {
      setAnchor(null);
      action();
    };
  return (
    <>
      <IconButton
        size="small"
        aria-label={t('widgets.entityFolders.folderMore', 'More for folder {{name}}', { name })}
        aria-haspopup="menu"
        data-testid="entity-folder-more"
        onClick={(event) => {
          setAnchor(event.currentTarget);
        }}
      >
        <MoreHorizIcon fontSize="small" />
      </IconButton>
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={() => {
          setAnchor(null);
        }}
      >
        <MenuItem onClick={run(onRename)}>{t('widgets.entityFolders.rename', 'Rename')}</MenuItem>
        <MenuItem
          disabled={!canMoveUp}
          onClick={run(() => {
            onMove('up');
          })}
        >
          {t('widgets.entityFolders.moveUp', 'Move up')}
        </MenuItem>
        <MenuItem
          disabled={!canMoveDown}
          onClick={run(() => {
            onMove('down');
          })}
        >
          {t('widgets.entityFolders.moveDown', 'Move down')}
        </MenuItem>
        <Divider />
        <MenuItem onClick={run(onDelete)}>{t('widgets.entityFolders.delete', 'Delete…')}</MenuItem>
      </Menu>
    </>
  );
}
