import { useMemo, type ReactNode } from 'react';

import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Snackbar from '@mui/material/Snackbar';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import {
  folderOfEntity,
  useEntityFolderMutations,
  useEntityFolders,
  useEntityPins,
  type EntityFolder,
  type EntityFolderKind,
} from '@/entities/entity-folder';
import { serverErrorOf } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';
import { EntityRowSlotProvider, type EntityListItem, type EntityRowSlot } from '@/shared/ui/EntityCardList';

import { entityFolderAccess } from '../lib/access';
import { useEntityFolderDnd } from '../lib/entityDnd';
import type { FolderReorder } from '../lib/useFolderReorder';

import { DragHandle } from './DragHandle';
import { MoveToFolderControl, type StepControl } from './MoveToFolderControl';
import { PinToggle } from './PinToggle';

export interface EntityFolderScopeProps {
  readonly projectId: string | undefined;
  readonly kind: EntityFolderKind;
  /** The caller's project permissions: moving an entity needs the list's update permission, and without it the table still shows the folder, read-only. */
  readonly permissions: ReadonlySet<string>;
  readonly children: ReactNode;
}

/**
 * Wrap a list page's own list (not its public tabs) and every card and every
 * table row below it gets the project's folders: a "Move to folder" control
 * in the card's corner, and a "Folder" column in the table. One scope serves
 * the five lists; the page only says which kind it is.
 */
export function EntityFolderScope({ projectId, kind, permissions, children }: EntityFolderScopeProps): ReactNode {
  // No project (the public project's own lists pass none) means no folders to move into.
  const canUpdate = entityFolderAccess(kind, permissions).canUpdate && projectId !== undefined;
  const folders = useEntityFolders(projectId, kind).data;
  const { move, pin } = useEntityFolderMutations(projectId, kind);
  const { mutate: moveEntity } = move;
  const { mutate: pinEntity } = pin;
  const pinnedIds = useEntityPins(projectId, kind).data;
  const reorder = useEntityFolderDnd()?.reorder;
  const slot = useMemo<EntityRowSlot>(() => {
    const known = folders ?? [];
    const pinned = new Set(pinnedIds ?? []);
    const control = (item: EntityListItem, current: EntityFolder | undefined): ReactNode => {
      const entityId = Number(item.id);
      if (!Number.isInteger(entityId)) return null;
      return (
        <Box sx={controlsSx}>
          <DragHandle entityId={entityId} entityName={item.name} />
          {projectId !== undefined && (
            <PinToggle
              entityName={item.name}
              pinned={pinned.has(entityId)}
              onToggle={(next) => {
                pinEntity({ entityId, pinned: next });
              }}
            />
          )}
          {canUpdate && (
            <MoveToFolderControl
              entityName={item.name}
              folders={known}
              current={current}
              onMove={(folderId) => {
                moveEntity({ entityId, folderId });
              }}
              step={stepOf(reorder, entityId)}
            />
          )}
        </Box>
      );
    };
    return {
      columnLabel: t('widgets.entityFolders.column', 'Folder'),
      renderCardAction: (item) => control(item, folderOfEntity(known, item.id)),
      isPinned: (item) => pinned.has(Number(item.id)),
      renderCell: (item) => {
        const current = folderOfEntity(known, item.id);
        return (
          <Box sx={cellSx}>
            {current !== undefined && (
              <Typography variant="bodyMedium" data-testid="entity-folder-cell-name" sx={nameSx}>
                <FolderOutlinedIcon sx={iconSx} />
                {current.name}
              </Typography>
            )}
            {control(item, current)}
          </Box>
        );
      },
    };
  }, [folders, pinnedIds, projectId, canUpdate, moveEntity, pinEntity, reorder]);
  return (
    <EntityRowSlotProvider value={slot}>
      {children}
      <Snackbar
        open={move.isError || pin.isError}
        autoHideDuration={6000}
        onClose={() => {
          move.reset();
          pin.reset();
        }}
      >
        <Alert
          severity="error"
          data-testid="entity-folder-move-error"
          onClose={() => {
            move.reset();
          pin.reset();
          }}
        >
          {serverErrorOf(move.error ?? pin.error) ?? t('widgets.entityFolders.moveFailed', 'The item could not be moved to that folder.')}
        </Alert>
      </Snackbar>
    </EntityRowSlotProvider>
  );
}

/** Move up / Move down for a card of the open folder in its own order; undefined otherwise. */
function stepOf(reorder: FolderReorder | undefined, entityId: number): StepControl | undefined {
  const index = reorder?.indexOf(entityId) ?? -1;
  if (reorder === undefined || index === -1) return undefined;
  return {
    canUp: index > 0,
    canDown: index < reorder.count - 1,
    onStep: (direction) => {
      reorder.step(entityId, direction);
    },
  };
}

const controlsSx: SxProps<Theme> = { display: 'flex', alignItems: 'center' };

const cellSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.5),
  minWidth: 0,
});

const nameSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '0.25rem',
  minWidth: 0,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const iconSx: SxProps<Theme> = { width: '1rem', height: '1rem', flexShrink: 0 };
