import { useCallback, useMemo, useRef, useState, type ReactNode } from 'react';

import { closestCenter, DndContext, DragOverlay, pointerWithin, rectIntersection, type CollisionDetection, type DragEndEvent } from '@dnd-kit/core';
import { SortableContext } from '@dnd-kit/sortable';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Snackbar from '@mui/material/Snackbar';
import type { SxProps, Theme } from '@mui/material/styles';

import { useEntityFolderMutations, useEntityFolders, type EntityFolderKind } from '@/entities/entity-folder';
import { t } from '@/shared/i18n';
import { buildDndAnnouncements, dndInstructions, droppableKeyboardCoordinates, useGroupDnd } from '@/shared/lib/dnd';
import { visuallyHidden } from '@/shared/lib/visuallyHidden';

import { entityFolderAccess } from '../lib/access';
import { EntityFolderDndContext, entityAnnouncementLabels, entityDndPolicy, entityDragId, entityIdOfDragId } from '../lib/entityDnd';
import type { FolderOrder } from '../lib/useFolderOrder';
import { useFolderReorder, type FolderReorder } from '../lib/useFolderReorder';

export interface EntityFolderDndProps {
  readonly projectId: string | undefined;
  readonly kind: EntityFolderKind;
  /** The caller's project permissions: moving an entity needs the list's update permission; without it the page has no drag and drop. */
  readonly permissions: ReadonlySet<string>;
  /** The ids picked on the list. A drag that starts on one of them carries every one. */
  readonly selectedIds?: readonly string[];
  /** The open folder in its own order (`useFolderOrder().folderOrder`): the cards then also drop on each other to take a new place in it. */
  readonly folderOrder?: FolderOrder | undefined;
  readonly children: ReactNode;
}

const isCard = (id: string | number): boolean => entityIdOfDragId(id) !== undefined;

/**
 * The pointer first (the row under the cursor), then the overlap with a folder,
 * which is all a keyboard drag has; with the open folder sortable, anywhere
 * else in the list is the place of the nearest card.
 */
const collisions: CollisionDetection = (args) => {
  const underPointer = pointerWithin(args);
  if (underPointer.length > 0) return underPointer;
  const zones = rectIntersection({ ...args, droppableContainers: args.droppableContainers.filter((container) => !isCard(container.id)) });
  if (zones.length > 0) return zones;
  // The dragged card is one of them: over its own place, a drop changes nothing.
  const cards = args.droppableContainers.filter((container) => isCard(container.id));
  return cards.length > 0 ? closestCenter({ ...args, droppableContainers: cards }) : [];
};

/** A drop on another card, with the folder sortable: the dragged card takes that card's place. True when it was one. */
function dropOnCard(event: DragEndEvent, reorder: FolderReorder | undefined): boolean {
  const target = event.over === null ? undefined : entityIdOfDragId(event.over.id);
  const dragged = entityIdOfDragId(event.active.id);
  if (reorder === undefined || target === undefined || dragged === undefined) return false;
  if (target !== dragged) reorder.moveTo(dragged, reorder.indexOf(target));
  return true;
}

/**
 * Drag and drop of entities into the folders of a list (ADR-0051 M2). Wrap the
 * page's sidebar AND its list: the folder rows in the sidebar are the drop
 * zones, the handle on each card or row is the draggable. Pointer and keyboard
 * (Space, arrows, Space) both work, a screen reader hears each step, and the
 * result is also announced once the server has answered. Without update rights
 * on the list it offers no handle and no drop zone, with the same tree.
 */
export function EntityFolderDnd({ projectId, kind, permissions, selectedIds = [], folderOrder, children }: EntityFolderDndProps): ReactNode {
  const enabled = entityFolderAccess(kind, permissions).canUpdate && projectId !== undefined;
  // The same tree whatever the permissions say: permissions load after the first paint, and a wrapper that appeared only once they
  // arrived would remount the page below it (and drop what a person had already started, such as a picked import file).
  return (
    <ActiveDnd projectId={projectId} kind={kind} selectedIds={selectedIds} enabled={enabled} folderOrder={enabled ? folderOrder : undefined}>
      {children}
    </ActiveDnd>
  );
}

function ActiveDnd({
  projectId,
  kind,
  selectedIds,
  enabled,
  folderOrder,
  children,
}: Omit<EntityFolderDndProps, 'permissions'> & { readonly selectedIds: readonly string[]; readonly enabled: boolean }): ReactNode {
  const folders = useEntityFolders(projectId, kind).data;
  const { moveQuiet, refreshEntities } = useEntityFolderMutations(projectId, kind);
  const names = useRef(new Map<number, string>());
  const register = useCallback((entityId: number, name: string): void => {
    names.current.set(entityId, name);
  }, []);
  const nameOf = useCallback((entityId: number): string => names.current.get(entityId) ?? String(entityId), []);
  const [status, setStatus] = useState('');
  const [error, setError] = useState<string | undefined>(undefined);
  const moved = useRef(0);

  const known = useMemo(() => folders ?? [], [folders]);
  const policy = useMemo(
    () =>
      entityDndPolicy({
        folders: known,
        selectedIds,
        nameOf,
        move: async (entityId, folderId) => {
          await moveQuiet(entityId, folderId);
          moved.current += 1;
        },
      }),
    [known, selectedIds, nameOf, moveQuiet],
  );
  const reorder = useFolderReorder({ projectId, kind, folderOrder, nameOf, announce: setStatus, fail: setError });
  const labels = useMemo(() => entityAnnouncementLabels(known, nameOf, reorder), [known, nameOf, reorder]);
  const announcements = useMemo(() => buildDndAnnouncements(labels), [labels]);
  const dnd = useGroupDnd(policy, { toastError: setError, toastSuccess: setStatus, coordinateGetter: droppableKeyboardCoordinates });
  const { handleDragEnd } = dnd;
  // How many items the drag in flight carries, kept for the end of the drop: the state is reset by then.
  const carried = useRef(0);
  carried.current = dnd.isDragging ? dnd.draggedItems.length : carried.current;

  const onDragEnd = useCallback(
    async (event: DragEndEvent): Promise<void> => {
      moved.current = 0;
      setError(undefined);
      setStatus('');
      await handleDragEnd(event);
      if (dropOnCard(event, reorder)) return;
      if (moved.current > 0) refreshEntities();
      const target = event.over === null ? undefined : labels.targetName(event.over.id);
      // A drop of several items says how many moved (the hook already did); a single one says which.
      if (carried.current === 1 && moved.current === 1 && target !== undefined) {
        setStatus(t('widgets.entityFolders.dnd.moved', 'Moved {{name}} to {{target}}.', { name: labels.itemName(event.active.id) ?? '', target }));
      }
    },
    [handleDragEnd, refreshEntities, labels, reorder],
  );

  const value = useMemo(
    () => ({
      isDragging: dnd.isDragging,
      draggedFromFolder: dnd.draggedFrom !== null,
      getDropAreaState: dnd.getDropAreaState,
      register,
      reorder,
    }),
    [dnd.isDragging, dnd.draggedFrom, dnd.getDropAreaState, register, reorder],
  );
  const sortableIds = useMemo(() => (folderOrder?.ids ?? []).map(entityDragId), [folderOrder]);
  const overlayLabel = dnd.draggedItems.length > 1 ? t('widgets.entityFolders.dnd.overlayMany', '{{count}} items', { count: dnd.draggedItems.length }) : dnd.draggedItems[0]?.name;

  return (
    <DndContext
      sensors={dnd.sensors}
      collisionDetection={collisions}
      onDragStart={dnd.handleDragStart}
      onDragOver={dnd.handleDragOver}
      onDragEnd={(event) => void onDragEnd(event)}
      onDragCancel={dnd.handleDragCancel}
      accessibility={{
        announcements,
        screenReaderInstructions: dndInstructions(instructionsText(reorder !== undefined)),
      }}
    >
      <EntityFolderDndContext.Provider value={enabled ? value : undefined}>
        <SortableContext items={sortableIds}>{children}</SortableContext>
      </EntityFolderDndContext.Provider>
      <DragOverlay dropAnimation={null}>
        {dnd.isDragging && overlayLabel !== undefined ? (
          <Box data-testid="entity-folder-drag-overlay" sx={overlaySx}>
            {overlayLabel}
          </Box>
        ) : null}
      </DragOverlay>
      <Box component="output" data-testid="entity-folder-dnd-status" sx={visuallyHidden}>
        {status}
      </Box>
      <Snackbar
        open={error !== undefined}
        autoHideDuration={6000}
        onClose={() => {
          setError(undefined);
        }}
      >
        <Alert
          severity="error"
          data-testid="entity-folder-dnd-error"
          onClose={() => {
            setError(undefined);
          }}
        >
          {error}
        </Alert>
      </Snackbar>
    </DndContext>
  );
}

function instructionsText(sortable: boolean): string {
  return sortable
    ? t(
        'widgets.entityFolders.order.instructions',
        'To move this item, press Space to pick it up, use the arrow keys to choose a place in this folder or another folder, then press Space to drop it. Escape cancels. The item menu also has Move up and Move down.',
      )
    : t('widgets.entityFolders.dnd.instructions', 'To move this item to a folder, press Space to pick it up, use the arrow keys to choose a folder, then press Space to drop it. Escape cancels.');
}

const overlaySx: SxProps<Theme> = (theme: Theme) => ({
  padding: theme.spacing(0.75, 1.5),
  borderRadius: theme.vars.shape.radiusLg,
  backgroundColor: theme.vars.palette.background.paper,
  border: `1px solid ${theme.vars.palette.primary.main}`,
  boxShadow: theme.shadows[4],
  maxWidth: '16rem',
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
});
