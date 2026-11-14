import { useMemo } from 'react';

import { entityIdsInFolder, useEntityFolders, type EntityFolderKind } from '@/entities/entity-folder';
import { useRailFolderSelection } from '@/shared/ui/EntityRail';

export interface FolderFilter {
  /** A folder is open, so the list narrows to it. */
  readonly active: boolean;
  /** Whether an entity belongs in the narrowed list; true for everything when no folder is open. */
  readonly includes: (entityId: string | number) => boolean;
}

/**
 * The open folder as a client-side filter, for the lists whose endpoint has no
 * folder parameter (skills, toolkits, MCPs). The agents and pipelines lists
 * send `folder_id` to the server instead.
 */
export function useFolderFilter(projectId: string | undefined, kind: EntityFolderKind): FolderFilter {
  const { selectedFolderId } = useRailFolderSelection();
  const folders = useEntityFolders(projectId, kind).data;
  return useMemo(() => {
    const ids = entityIdsInFolder(folders ?? [], selectedFolderId);
    if (selectedFolderId === undefined) return { active: false, includes: () => true };
    return { active: true, includes: (entityId) => ids?.has(Number(entityId)) === true };
  }, [folders, selectedFolderId]);
}
