import { useEffect, useState, type ReactNode } from 'react';

import { useDroppable } from '@dnd-kit/core';
import AddIcon from '@mui/icons-material/Add';
import PushPinOutlinedIcon from '@mui/icons-material/PushPinOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import ListItemButton from '@mui/material/ListItemButton';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import {
  neighboursForStep,
  isPinnedGroupId,
  pinnedGroupId,
  useEntityFolderMutations,
  useEntityFolders,
  useEntityPins,
  type EntityFolder,
  type EntityFolderKind,
} from '@/entities/entity-folder';
import { t } from '@/shared/i18n';
import { useRailFolderSelection } from '@/shared/ui/EntityRail';

import { entityFolderAccess } from '../lib/access';
import { ROOT_DROP_ID, useEntityFolderDnd } from '../lib/entityDnd';

import { DeleteDialogHost, NameDialogHost, type NameTarget } from './FolderDialogs';
import { FolderRow, type FolderRowProps } from './FolderRow';

export interface EntityFolderPanelProps {
  readonly projectId: string | undefined;
  readonly kind: EntityFolderKind;
  /** The caller's project permissions: the list's create permission adds folders, its update permission renames, reorders and deletes them. */
  readonly permissions: ReadonlySet<string>;
  /** False draws nothing, for the tabs and projects whose list has no folders (the public feeds, the Teams tab). */
  readonly enabled?: boolean;
}

/**
 * The Folders section of a list's Filters panel: "All", then the project's
 * folders of this list, one click to filter the list to a folder. People with
 * the list's create and update permissions can add, rename, reorder and
 * delete folders; deleting one returns its entities to the root.
 */
export function EntityFolderPanel({ enabled = true, ...rest }: EntityFolderPanelProps): ReactNode {
  return enabled ? <FolderPanelBody {...rest} /> : null;
}

function FolderPanelBody({ projectId, kind, permissions }: Omit<EntityFolderPanelProps, 'enabled'>): ReactNode {
  const { canCreate, canUpdate } = entityFolderAccess(kind, permissions);
  const query = useEntityFolders(projectId, kind);
  const folders = query.data ?? [];
  const { selectedFolderId, selectFolder } = useRailFolderSelection();
  const { reorder } = useEntityFolderMutations(projectId, kind);
  const [naming, setNaming] = useState<NameTarget | undefined>(undefined);
  const [deleting, setDeleting] = useState<EntityFolder | undefined>(undefined);

  // The param is shared by the five lists: an id from another list, or a
  // deleted folder, matches nothing here, so drop it instead of an empty list.
  const stale =
    selectedFolderId !== undefined && !isPinnedGroupId(selectedFolderId) && query.isSuccess && !folders.some((folder) => folder.id === selectedFolderId);
  useEffect(() => {
    if (stale) selectFolder(undefined);
  }, [stale, selectFolder]);

  const menuFor = (folder: EntityFolder): ComponentMenu | undefined => {
    if (!canUpdate) return undefined;
    const up = neighboursForStep(folders, folder.id, 'up');
    const down = neighboursForStep(folders, folder.id, 'down');
    return {
      canMoveUp: up !== undefined,
      canMoveDown: down !== undefined,
      onRename: () => {
        setNaming(folder);
      },
      onMove: (direction) => {
        const step = direction === 'up' ? up : down;
        if (step !== undefined) reorder.mutate({ folderId: folder.id, above: step.above, below: step.below });
      },
      onDelete: () => {
        setDeleting(folder);
      },
    };
  };

  return (
    <Box
      component="section"
      aria-label={t('widgets.entityFolders.title', 'Folders')}
      data-testid="entity-folder-panel"
      sx={panelSx}
    >
      <Box sx={headerSx}>
        <Typography variant="labelMedium">{t('widgets.entityFolders.title', 'Folders')}</Typography>
        {canCreate && (
          <IconButton
            size="small"
            aria-label={t('widgets.entityFolders.create', 'New folder')}
            data-testid="entity-folder-create"
            onClick={() => {
              setNaming('create');
            }}
          >
            <AddIcon fontSize="small" />
          </IconButton>
        )}
      </Box>
      <ListItemButton
        selected={selectedFolderId === undefined}
        data-testid="entity-folder-all"
        onClick={() => {
          selectFolder(undefined);
        }}
      >
        <Typography variant="bodyMedium">{t('widgets.entityFolders.all', 'All')}</Typography>
      </ListItemButton>
      {projectId !== undefined && (
        <PinnedRow
          projectId={projectId}
          kind={kind}
        />
      )}
      {query.isSuccess && folders.length === 0 && <EmptyNote canCreate={canCreate} />}
      {folders.map((folder) => (
        <FolderRow
          key={folder.id}
          folder={folder}
          selected={folder.id === selectedFolderId}
          menu={menuFor(folder)}
          onSelect={() => {
            selectFolder(folder.id);
          }}
        />
      ))}
      <RootDropZone />
      {naming !== undefined && (
        <NameDialogHost
          projectId={projectId}
          kind={kind}
          target={naming}
          onClose={() => {
            setNaming(undefined);
          }}
        />
      )}
      {deleting !== undefined && (
        <DeleteDialogHost
          projectId={projectId}
          kind={kind}
          folder={deleting}
          onClose={() => {
            setDeleting(undefined);
          }}
        />
      )}
    </Box>
  );
}

/** The personal Pinned group: one click filters the list to what the caller pinned. */
function PinnedRow({ projectId, kind }: { readonly projectId: string; readonly kind: EntityFolderKind }): ReactNode {
  const pinnedCount = useEntityPins(projectId, kind).data?.length ?? 0;
  const { selectedFolderId, selectFolder } = useRailFolderSelection();
  return (
    <ListItemButton
      selected={selectedFolderId === pinnedGroupId(kind)}
      data-testid="entity-folder-pinned"
      onClick={() => {
        selectFolder(pinnedGroupId(kind));
      }}
    >
      <PushPinOutlinedIcon sx={pinIconSx} />
      <Typography variant="bodyMedium" sx={pinnedNameSx}>
        {t('widgets.entityFolders.pinned', 'Pinned')}
      </Typography>
      <Typography variant="bodySmall" sx={countSx}>
        {pinnedCount}
      </Typography>
    </ListItemButton>
  );
}

/**
 * "No folder", a drop zone that exists only while an entity that sits in a
 * folder is being dragged: the way back to the root, for the pointer and for
 * the keyboard alike.
 */
function RootDropZone(): ReactNode {
  const dnd = useEntityFolderDnd();
  if (dnd === undefined || !dnd.isDragging || !dnd.draggedFromFolder) return null;
  return <ActiveRootDropZone />;
}

function ActiveRootDropZone(): ReactNode {
  const { setNodeRef, isOver } = useDroppable({ id: ROOT_DROP_ID });
  return (
    <Box
      ref={setNodeRef}
      data-testid="entity-folder-root-drop"
      data-drop-over={isOver ? 'true' : undefined}
      sx={isOver ? rootOverSx : rootSx}
    >
      <Typography variant="bodyMedium">{t('widgets.entityFolders.noFolderDrop', 'Drop here to take it out of its folder')}</Typography>
    </Box>
  );
}

type ComponentMenu = NonNullable<FolderRowProps['menu']>;

function EmptyNote({ canCreate }: { readonly canCreate: boolean }): ReactNode {
  return (
    <Typography variant="bodySmall" data-testid="entity-folder-empty" sx={emptySx}>
      {canCreate
        ? t('widgets.entityFolders.empty', 'No folders yet. Use + to create one, then move items into it.')
        : t('widgets.entityFolders.emptyReadOnly', 'No folders yet.')}
    </Typography>
  );
}

const panelSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(0.5),
  flexShrink: 0,
});

const headerSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'space-between' };

const emptySx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.secondary,
  padding: theme.spacing(0, 1),
});

const pinIconSx: SxProps<Theme> = { width: '1.125rem', height: '1.125rem', flexShrink: 0, marginRight: '0.5rem' };

const pinnedNameSx: SxProps<Theme> = { flex: 1, minWidth: 0 };

const countSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });

const rootSx: SxProps<Theme> = (theme: Theme) => ({
  padding: theme.spacing(1),
  border: `1px dashed ${theme.vars.palette.divider}`,
  borderRadius: theme.vars.shape.radiusSm,
  color: theme.vars.palette.text.secondary,
});

const rootOverSx: SxProps<Theme> = (theme: Theme) => ({
  padding: theme.spacing(1),
  border: `1px dashed ${theme.vars.palette.primary.main}`,
  borderRadius: theme.vars.shape.radiusSm,
  backgroundColor: theme.vars.palette.action.hover,
});
