import type { EntityFolder } from '@/shared/api/generated/model';

/** The five lists that have folders (issue 2048). Pipelines are agents of the pipeline type, but their list has its own folders. */
export type EntityFolderKind = 'agent' | 'pipeline' | 'skill' | 'toolkit' | 'mcp';

/** The folder an entity sits in, or undefined at the root. Entity ids are numbers on the wire and strings in the list rows. */
export function folderOfEntity(folders: readonly EntityFolder[], entityId: string | number): EntityFolder | undefined {
  const id = Number(entityId);
  return folders.find((folder) => folder.item_ids.includes(id));
}

/** The ids inside a folder, as a set, for the lists that filter on the client (skills, toolkits, MCPs). */
export function entityIdsInFolder(
  folders: readonly EntityFolder[],
  folderId: string | undefined,
): ReadonlySet<number> | undefined {
  if (folderId === undefined || folderId === '') return undefined;
  return new Set(folders.find((folder) => folder.id === folderId)?.item_ids ?? []);
}

/**
 * The neighbour ids of a drop that puts `folderId` one step up or down. The
 * server turns them into a position and rebalances when they are adjacent, so
 * the client never does position arithmetic. `undefined` when the folder is
 * already at that end.
 */
export function neighboursForStep(
  folders: readonly EntityFolder[],
  folderId: string,
  direction: 'up' | 'down',
): { readonly above: string | null; readonly below: string | null } | undefined {
  // `folders` is in the server's order, which is the order the person sees.
  const index = folders.findIndex((folder) => folder.id === folderId);
  if (index === -1) return undefined;
  const rest = folders.filter((folder) => folder.id !== folderId);
  const target = direction === 'up' ? index - 1 : index + 1;
  if (target < 0 || target > rest.length) return undefined;
  return { above: rest[target - 1]?.id ?? null, below: rest[target]?.id ?? null };
}
