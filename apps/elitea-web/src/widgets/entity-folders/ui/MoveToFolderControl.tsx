import { useState, type MouseEvent, type ReactNode } from 'react';

import CheckIcon from '@mui/icons-material/Check';
import FolderOpenOutlinedIcon from '@mui/icons-material/FolderOpenOutlined';
import IconButton from '@mui/material/IconButton';
import ListItemIcon from '@mui/material/ListItemIcon';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Tooltip from '@mui/material/Tooltip';

import type { EntityFolder } from '@/entities/entity-folder';
import { t } from '@/shared/i18n';

interface MoveToFolderControlProps {
  /** The row's entity name, for the control's accessible name. */
  readonly entityName: string;
  readonly folders: readonly EntityFolder[];
  /** The folder the entity sits in now; undefined at the root. */
  readonly current: EntityFolder | undefined;
  /** The chosen folder id, or null for the root. */
  readonly onMove: (folderId: string | null) => void;
}

/**
 * "Move to folder" on one row: a button that opens the project's folders of
 * this list, the current one ticked, plus a way back to the root. It sits
 * inside a clickable card or row, so its clicks never reach the card.
 */
export function MoveToFolderControl({ entityName, folders, current, onMove }: MoveToFolderControlProps): ReactNode {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const label = t('widgets.entityFolders.moveTo', 'Move {{name}} to a folder', { name: entityName });
  const close = (): void => {
    setAnchor(null);
  };
  const choose = (folderId: string | null): void => {
    close();
    onMove(folderId);
  };
  return (
    <>
      <Tooltip title={t('widgets.entityFolders.moveToShort', 'Move to folder')}>
        <IconButton
          size="small"
          aria-label={label}
          aria-haspopup="menu"
          data-testid="move-to-folder-button"
          onClick={(event: MouseEvent<HTMLElement>) => {
            event.stopPropagation();
            setAnchor(event.currentTarget);
          }}
        >
          <FolderOpenOutlinedIcon fontSize="small" />
        </IconButton>
      </Tooltip>
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={close}
        // A portal's events still bubble through React to the card behind it.
        onClick={(event) => {
          event.stopPropagation();
        }}
        slotProps={{ list: { 'aria-label': label } }}
      >
        {folders.length === 0 && (
          <MenuItem disabled>
            {t('widgets.entityFolders.noneYet', 'No folders yet. Create one in the Filters panel.')}
          </MenuItem>
        )}
        {folders.map((folder) => (
          <MenuItem
            key={folder.id}
            selected={folder.id === current?.id}
            data-testid="move-to-folder-option"
            onClick={() => {
              choose(folder.id);
            }}
          >
            {folder.id === current?.id && (
              <ListItemIcon>
                <CheckIcon fontSize="small" />
              </ListItemIcon>
            )}
            {folder.name}
          </MenuItem>
        ))}
        {current !== undefined && (
          <MenuItem
            data-testid="move-to-root-option"
            onClick={() => {
              choose(null);
            }}
          >
            {t('widgets.entityFolders.removeFromFolder', 'Remove from folder')}
          </MenuItem>
        )}
      </Menu>
    </>
  );
}
