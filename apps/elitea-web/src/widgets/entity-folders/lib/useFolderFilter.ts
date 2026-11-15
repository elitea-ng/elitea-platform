import { useEffect, useMemo } from 'react';

import { entityIdsInFolder, PINNED_GROUP_ID, useEntityFolders, useEntityPins, type EntityFolderKind } from '@/entities/entity-folder';
import { useRailFolderSelection } from '@/shared/ui/EntityRail';

export interface FolderFilter {
  /** A folder is open and exists in this list, so the list narrows to it. */
  readonly active: boolean;
  /** The open folder's id while it is valid; undefined otherwise. The agents and pipelines lists send it to the server. */
  readonly folderId: string | undefined;
  /** Whether an entity belongs in the narrowed list; true for everything when no valid folder is open. */
  readonly includes: (entityId: string | number) => boolean;
  /** The personal Pinned group is open; `folderId` is then the reserved PINNED_GROUP_ID, which the list helpers turn into the server's `pinned` flag. */
  readonly pinned: boolean;
  /** True while a folder is open but its folders are not known yet, so a page does not show an "empty" state for it. */
  readonly pending: boolean;
}

/**
 * The open folder as a filter. It applies only once the folders query has
 * SUCCEEDED and the folder is in this list: while the query is off (the public
 * project) or failed, or when the `entity_folder` param is a stale id (another
 * list's, or a deleted folder), the list shows everything, and a stale param
 * is cleared, so a 5xx or an old link never blanks a list.
 */
export function useFolderFilter(projectId: string | undefined, kind: EntityFolderKind): FolderFilter {
  const { selectedFolderId, selectFolder } = useRailFolderSelection();
  const query = useEntityFolders(projectId, kind);
  const folders = query.data;
  const pins = useEntityPins(projectId, kind).data;
  const pinnedOpen = selectedFolderId === PINNED_GROUP_ID;
  const known = folders?.some((folder) => folder.id === selectedFolderId) === true;
  const stale = selectedFolderId !== undefined && !pinnedOpen && query.isSuccess && !known;
  useEffect(() => {
    if (stale) selectFolder(undefined);
  }, [stale, selectFolder]);
  return useMemo(() => {
    if (pinnedOpen && projectId !== undefined) {
      const ids = new Set(pins ?? []);
      return { active: true, pinned: true, folderId: PINNED_GROUP_ID, includes: (entityId) => ids.has(Number(entityId)), pending: pins === undefined };
    }
    if (selectedFolderId === undefined || !known) {
      return { active: false, pinned: false, folderId: undefined, includes: () => true, pending: selectedFolderId !== undefined && !pinnedOpen && query.isPending && query.fetchStatus !== 'idle' };
    }
    const ids = entityIdsInFolder(folders ?? [], selectedFolderId);
    return { active: true, pinned: false, folderId: selectedFolderId, includes: (entityId) => ids?.has(Number(entityId)) === true, pending: false };
  }, [folders, pins, pinnedOpen, projectId, selectedFolderId, known, query.isPending, query.fetchStatus]);
}

/** The `folder_id` (or, for the personal Pinned group, `pinned`) request param of the agents and pipelines lists, for a spread; empty with no valid open folder. */
export function folderIdParam(folderId: string | undefined): { readonly folder_id?: string; readonly pinned?: boolean } {
  if (folderId === undefined) return {};
  return folderId === PINNED_GROUP_ID ? { pinned: true } : { folder_id: folderId };
}
