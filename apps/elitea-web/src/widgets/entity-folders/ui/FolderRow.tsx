import type { ReactNode } from 'react';

import { useDroppable } from '@dnd-kit/core';

import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import Box from '@mui/material/Box';
import ListItemButton from '@mui/material/ListItemButton';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { EntityFolder } from '@/entities/entity-folder';

import { folderDropId, useEntityFolderDnd } from '../lib/entityDnd';

import { FolderRowMenu, type FolderRowMenuProps } from './FolderRowMenu';

export interface FolderRowProps {
  readonly folder: EntityFolder;
  readonly selected: boolean;
  /** The row's ⋯ menu, when the person may change folders; absent for read-only access. */
  readonly menu: Omit<FolderRowMenuProps, 'name'> | undefined;
  readonly onSelect: () => void;
}

/** One folder of the panel: a button that filters the list to it, its entity count, and the ⋯ menu. */
export function FolderRow(props: FolderRowProps): ReactNode {
  const dnd = useEntityFolderDnd();
  return dnd === undefined ? <FolderRowBody {...props} dropRef={undefined} highlighted={false} /> : <DroppableFolderRow {...props} />;
}

/** The row as a drop zone: highlighted while an entity that can go in this folder is dragged. */
function DroppableFolderRow(props: FolderRowProps): ReactNode {
  const dropId = folderDropId(props.folder.id);
  const { setNodeRef, isOver } = useDroppable({ id: dropId });
  const state = useEntityFolderDnd()?.getDropAreaState(dropId);
  return <FolderRowBody {...props} dropRef={setNodeRef} highlighted={state?.isValidDropTarget === true && isOver} />;
}

function FolderRowBody({ folder, selected, menu, onSelect, dropRef, highlighted }: FolderRowProps & { readonly dropRef: ((node: HTMLElement | null) => void) | undefined; readonly highlighted: boolean }): ReactNode {
  return (
    <Box ref={dropRef} data-testid="entity-folder-drop" data-drop-over={highlighted ? 'true' : undefined} sx={highlighted ? dropOverSx : rowSx}>
      <ListItemButton selected={selected} data-testid="entity-folder-row" sx={buttonSx} onClick={onSelect}>
        <FolderOutlinedIcon sx={iconSx} />
        <Typography variant="bodyMedium" sx={nameSx}>
          {folder.name}
        </Typography>
        <Typography variant="bodySmall" sx={countSx}>
          {folder.item_ids.length}
        </Typography>
      </ListItemButton>
      {menu !== undefined && <FolderRowMenu name={folder.name} {...menu} />}
    </Box>
  );
}

const rowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center' };

const dropOverSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  borderRadius: theme.vars.shape.radiusSm,
  outline: `2px solid ${theme.vars.palette.primary.main}`,
  backgroundColor: theme.vars.palette.action.hover,
});

const buttonSx: SxProps<Theme> = (theme: Theme) => ({ flex: 1, minWidth: 0, gap: theme.spacing(1) });

const iconSx: SxProps<Theme> = { width: '1.125rem', height: '1.125rem', flexShrink: 0 };

const nameSx: SxProps<Theme> = {
  flex: 1,
  minWidth: 0,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const countSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });
