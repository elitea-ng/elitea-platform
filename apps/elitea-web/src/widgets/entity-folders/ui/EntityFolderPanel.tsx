import { useEffect, useState, type ReactNode } from 'react';

import AddIcon from '@mui/icons-material/Add';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import ListItemButton from '@mui/material/ListItemButton';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import {
  neighboursForStep,
  useEntityFolderMutations,
  useEntityFolders,
  type EntityFolder,
  type EntityFolderKind,
} from '@/entities/entity-folder';
import { t } from '@/shared/i18n';
import { useRailFolderSelection } from '@/shared/ui/EntityRail';

import { entityFolderAccess } from '../lib/access';

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
    selectedFolderId !== undefined && query.isSuccess && !folders.some((folder) => folder.id === selectedFolderId);
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
      role="group"
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
