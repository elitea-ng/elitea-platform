import { createContext, useContext } from 'react';

import type { UniqueIdentifier } from '@dnd-kit/core';

import { folderOfEntity, type EntityFolder } from '@/entities/entity-folder';
import { t } from '@/shared/i18n';
import type { DndAnnouncementLabels, DndPolicy, DropAreaState } from '@/shared/lib/dnd';

/** The draggable id of an entity, and the droppable ids of a folder and of "no folder". */
const ENTITY_PREFIX = 'entity-';
const FOLDER_DROP_PREFIX = 'folder-drop-';
const ROOT_DROP_ID = 'folder-drop-root';

export const entityDragId = (entityId: number): string => `${ENTITY_PREFIX}${entityId}`;
export const folderDropId = (folderId: string): string => `${FOLDER_DROP_PREFIX}${folderId}`;
export { ROOT_DROP_ID };

/** The entity an `entity-<n>` draggable id names, or undefined for any other id. */
export function entityIdOfDragId(id: UniqueIdentifier): number | undefined {
  if (typeof id !== 'string' || !id.startsWith(ENTITY_PREFIX)) return undefined;
  const n = Number(id.slice(ENTITY_PREFIX.length));
  return Number.isInteger(n) ? n : undefined;
}

/** A dragged entity: its id and the name the screen reader and the overlay say. */
export interface EntityDragItem {
  readonly id: number;
  readonly name: string;
}

/** What the folder panel and the rows read from the page's drag-and-drop scope; absent where a page has no such scope. */
interface EntityFolderDndState {
  /** An entity drag is in flight. */
  readonly isDragging: boolean;
  /** Whether the dragged entity sits in a folder now, so the "No folder" zone has somewhere to take it from. */
  readonly draggedFromFolder: boolean;
  readonly getDropAreaState: (dropAreaId: string) => DropAreaState;
  /** Rows tell the scope their name, for the overlay and the announcements. */
  readonly register: (entityId: number, name: string) => void;
}

export const EntityFolderDndContext = createContext<EntityFolderDndState | undefined>(undefined);

export function useEntityFolderDnd(): EntityFolderDndState | undefined {
  return useContext(EntityFolderDndContext);
}

interface PolicyInputs {
  readonly folders: readonly EntityFolder[];
  /** The ids of the multi-selection; a drag that starts on one of them carries all of them. */
  readonly selectedIds: readonly string[];
  readonly nameOf: (entityId: number) => string;
  readonly move: (entityId: number, folderId: string | null) => Promise<unknown>;
}

const folderNameOf = (folders: readonly EntityFolder[], dropId: UniqueIdentifier): string | undefined => {
  if (dropId === ROOT_DROP_ID) return t('widgets.entityFolders.noFolder', 'No folder');
  if (typeof dropId !== 'string' || !dropId.startsWith(FOLDER_DROP_PREFIX)) return undefined;
  const id = dropId.slice(FOLDER_DROP_PREFIX.length);
  return folders.find((folder) => folder.id === id)?.name;
};

/** The entity lists' rules for `shared/lib/dnd`: an entity moves to another folder or back to the root, never onto where it already is. */
export function entityDndPolicy({ folders, selectedIds, nameOf, move }: PolicyInputs): DndPolicy<EntityDragItem, EntityFolder> {
  return {
    resolveDrag: (activeId) => {
      const entityId = entityIdOfDragId(activeId);
      if (entityId === undefined) return undefined;
      const selected = selectedIds.map(Number).filter((n) => Number.isInteger(n));
      const ids = selected.includes(entityId) && selected.length > 1 ? selected : [entityId];
      return {
        items: ids.map((id) => ({ id, name: nameOf(id) })),
        source: folderOfEntity(folders, entityId) ?? null,
      };
    },
    resolveDrop: (overId) => {
      if (overId === ROOT_DROP_ID) return { group: null, label: folderNameOf(folders, overId) ?? '' };
      const group = folders.find((folder) => folderDropId(folder.id) === overId);
      return group === undefined ? undefined : { group, label: group.name };
    },
    canMove: (item, target) => {
      const current = folderOfEntity(folders, item.id);
      return target === null ? current !== undefined : current?.id !== target.id;
    },
    move: (item, target) => move(item.id, target?.id ?? null),
    groupKey: (folder) => folder.id,
    messages: {
      moveError: t('widgets.entityFolders.moveFailed', 'The item could not be moved to that folder.'),
      moved: (count, label) =>
        count === 1
          ? t('widgets.entityFolders.movedOne', 'Moved 1 item to {{target}}.', { target: label })
          : t('widgets.entityFolders.movedMany', 'Moved {{count}} items to {{target}}.', { count, target: label }),
    },
  };
}

/** The screen reader's words for a drag of an entity: picked up, over a folder, dropped, cancelled. */
export function entityAnnouncementLabels(folders: readonly EntityFolder[], nameOf: (entityId: number) => string): DndAnnouncementLabels {
  return {
    itemName: (id) => {
      const entityId = entityIdOfDragId(id);
      return entityId === undefined ? undefined : nameOf(entityId);
    },
    targetName: (id) => folderNameOf(folders, id),
    pickedUp: (name) =>
      t('widgets.entityFolders.dnd.pickedUp', 'Picked up {{name}}. Use the arrow keys to choose a folder, Space to drop it, Escape to cancel.', { name }),
    over: (name, target) => t('widgets.entityFolders.dnd.over', '{{name}} is over {{target}}.', { name, target }),
    overNothing: (name) => t('widgets.entityFolders.dnd.overNothing', '{{name}} is not over a folder.', { name }),
    dropped: (name, target) => t('widgets.entityFolders.dnd.dropped', '{{name}} dropped on {{target}}.', { name, target }),
    cancelled: (name) => t('widgets.entityFolders.dnd.cancelled', 'Moving {{name}} cancelled.', { name }),
  };
}
