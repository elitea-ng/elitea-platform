import { useCallback, useMemo, useRef } from 'react';

import { neighboursAt, useEntityFolderMutations, type EntityFolderKind } from '@/entities/entity-folder';
import { serverErrorOf } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';

import type { FolderOrder } from './useFolderOrder';

/** What a card's handle and its menu read to rearrange the open folder. */
export interface FolderReorder {
  /** The entity's place on screen, from 0; -1 when it is not shown. */
  readonly indexOf: (entityId: number) => number;
  readonly count: number;
  /** Puts the entity at `toIndex` of the displayed order (a drop on another card). */
  readonly moveTo: (entityId: number, toIndex: number) => void;
  /** One step up or down (the menu's Move up / Move down). */
  readonly step: (entityId: number, direction: 'up' | 'down') => void;
}

interface ReorderInputs {
  readonly projectId: string | undefined;
  readonly kind: EntityFolderKind;
  readonly folderOrder: FolderOrder | undefined;
  readonly nameOf: (entityId: number) => string;
  /** The polite live region's text: what moved where. */
  readonly announce: (message: string) => void;
  readonly fail: (message: string) => void;
}

/** "position 2 of 5", the place a drag is over and where it landed. */
export function positionLabel(index: number, count: number): string {
  return t('widgets.entityFolders.order.position', 'position {{position}} of {{count}}', { position: index + 1, count });
}

/**
 * Rearranging the open folder (issue 2079): every drop or step sends the
 * displayed neighbours of the new place, the list shows it at once
 * (optimistic, rolled back on a refusal), and the screen reader hears where
 * the entity went. Undefined while the list is not in the folder's order.
 */
export function useFolderReorder({ projectId, kind, folderOrder, nameOf, announce, fail }: ReorderInputs): FolderReorder | undefined {
  const { mutate } = useEntityFolderMutations(projectId, kind).place;
  const latest = useRef({ announce, fail, nameOf });
  latest.current = { announce, fail, nameOf };

  const moveTo = useCallback(
    (entityId: number, toIndex: number): void => {
      if (folderOrder === undefined) return;
      const neighbours = neighboursAt(folderOrder.ids, entityId, toIndex);
      if (neighbours === undefined) return;
      const where = positionLabel(toIndex, folderOrder.ids.length);
      const name = latest.current.nameOf(entityId);
      mutate(
        { entityId, folderId: folderOrder.folderId, ...neighbours },
        {
          onSuccess: () => {
            latest.current.announce(t('widgets.entityFolders.order.moved', 'Moved {{name}} to {{target}}.', { name, target: where }));
          },
          onError: (error) => {
            latest.current.fail(serverErrorOf(error) ?? t('widgets.entityFolders.order.failed', 'The new order could not be saved.'));
          },
        },
      );
    },
    [folderOrder, mutate],
  );

  return useMemo(() => {
    if (folderOrder === undefined) return undefined;
    const { ids } = folderOrder;
    return {
      indexOf: (entityId) => ids.indexOf(entityId),
      count: ids.length,
      moveTo,
      step: (entityId, direction) => {
        const from = ids.indexOf(entityId);
        if (from !== -1) moveTo(entityId, direction === 'up' ? from - 1 : from + 1);
      },
    };
  }, [folderOrder, moveTo]);
}
