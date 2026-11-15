import { useCallback, useMemo, useRef, useState, type ReactNode } from 'react';

import { DndContext, DragOverlay, pointerWithin, rectIntersection, type CollisionDetection, type DragEndEvent } from '@dnd-kit/core';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Snackbar from '@mui/material/Snackbar';
import type { SxProps, Theme } from '@mui/material/styles';

import { useEntityFolderMutations, useEntityFolders, type EntityFolderKind } from '@/entities/entity-folder';
import { t } from '@/shared/i18n';
import { buildDndAnnouncements, dndInstructions, useGroupDnd } from '@/shared/lib/dnd';
import { visuallyHidden } from '@/shared/lib/visuallyHidden';

import { entityFolderAccess } from '../lib/access';
import { EntityFolderDndContext, entityAnnouncementLabels, entityDndPolicy } from '../lib/entityDnd';

export interface EntityFolderDndProps {
  readonly projectId: string | undefined;
  readonly kind: EntityFolderKind;
  /** The caller's project permissions: moving an entity needs the list's update permission; without it the page has no drag and drop. */
  readonly permissions: ReadonlySet<string>;
  /** The ids picked on the list. A drag that starts on one of them carries every one. */
  readonly selectedIds?: readonly string[];
  readonly children: ReactNode;
}

/** The pointer first (the row under the cursor), then the overlap, which is all a keyboard drag has. */
const collisions: CollisionDetection = (args) => {
  const underPointer = pointerWithin(args);
  return underPointer.length > 0 ? underPointer : rectIntersection(args);
};

/**
 * Drag and drop of entities into the folders of a list (ADR-0051 M2). Wrap the
 * page's sidebar AND its list: the folder rows in the sidebar are the drop
 * zones, the handle on each card or row is the draggable. Pointer and keyboard
 * (Space, arrows, Space) both work, a screen reader hears each step, and the
 * result is also announced once the server has answered. Without update rights
 * on the list it renders its children alone.
 */
export function EntityFolderDnd({ projectId, kind, permissions, selectedIds = [], children }: EntityFolderDndProps): ReactNode {
  const canUpdate = entityFolderAccess(kind, permissions).canUpdate && projectId !== undefined;
  return canUpdate ? (
    <ActiveDnd projectId={projectId} kind={kind} selectedIds={selectedIds}>
      {children}
    </ActiveDnd>
  ) : (
    children
  );
}

function ActiveDnd({ projectId, kind, selectedIds, children }: Omit<EntityFolderDndProps, 'permissions'> & { readonly selectedIds: readonly string[] }): ReactNode {
  const folders = useEntityFolders(projectId, kind).data;
  const { move } = useEntityFolderMutations(projectId, kind);
  const { mutateAsync: moveAsync } = move;
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
          await moveAsync({ entityId, folderId });
          moved.current += 1;
        },
      }),
    [known, selectedIds, nameOf, moveAsync],
  );
  const labels = useMemo(() => entityAnnouncementLabels(known, nameOf), [known, nameOf]);
  const announcements = useMemo(() => buildDndAnnouncements(labels), [labels]);
  const dnd = useGroupDnd(policy, { toastError: setError, toastSuccess: setStatus });
  const { handleDragEnd } = dnd;

  const onDragEnd = useCallback(
    async (event: DragEndEvent): Promise<void> => {
      moved.current = 0;
      setError(undefined);
      setStatus('');
      await handleDragEnd(event);
      const target = event.over === null ? undefined : labels.targetName(event.over.id);
      // A drop of several items says how many moved; a single one says which.
      if (moved.current === 1 && target !== undefined) {
        setStatus(t('widgets.entityFolders.dnd.moved', 'Moved {{name}} to {{target}}.', { name: labels.itemName(event.active.id) ?? '', target }));
      }
    },
    [handleDragEnd, labels],
  );

  const value = useMemo(
    () => ({
      isDragging: dnd.isDragging,
      draggedFromFolder: dnd.draggedFrom !== null,
      getDropAreaState: dnd.getDropAreaState,
      register,
    }),
    [dnd.isDragging, dnd.draggedFrom, dnd.getDropAreaState, register],
  );
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
        screenReaderInstructions: dndInstructions(
          t('widgets.entityFolders.dnd.instructions', 'To move this item to a folder, press Space to pick it up, use the arrow keys to choose a folder, then press Space to drop it. Escape cancels.'),
        ),
      }}
    >
      <EntityFolderDndContext.Provider value={value}>{children}</EntityFolderDndContext.Provider>
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
