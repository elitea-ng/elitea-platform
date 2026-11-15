import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import {
  createEntityFolder,
  deleteEntityFolder,
  listEntityFolders,
  moveEntityToFolder,
  setEntityPin,
  updateEntityFolder,
} from '@/shared/api/generated/applications/applications';
import type { EntityFolder } from '@/shared/api/generated/model';
import { unwrapListPage } from '@/shared/api/unwrap';

import type { EntityFolderKind } from './kinds';

const ROOT = ['entity-folders'] as const;

interface EntityFolderData {
  readonly folders: EntityFolder[];
  /** The ids the caller pinned in this list (private to them). */
  readonly pinnedIds: readonly number[];
}

function useEntityFolderData<T>(
  projectId: string | undefined,
  kind: EntityFolderKind,
  select: (data: EntityFolderData) => T,
): UseQueryResult<T> {
  return useQuery<EntityFolderData, Error, T>({
    queryKey: [...ROOT, projectId, kind],
    enabled: projectId !== undefined,
    queryFn: async ({ signal }) => {
      const response = await listEntityFolders(projectId ?? '', kind, { signal });
      const pinned: unknown = response.status === 200 ? response.data.pinned_ids : undefined;
      return {
        folders: unwrapListPage<EntityFolder>(response, 'listEntityFolders').rows,
        pinnedIds: Array.isArray(pinned) ? (pinned as number[]) : [],
      };
    },
    select,
  });
}

const foldersOf = (data: EntityFolderData): EntityFolder[] => data.folders;
const pinnedIdsOf = (data: EntityFolderData): readonly number[] => data.pinnedIds;

/** The folders of one list, highest position first. Disabled until the project is known. */
export function useEntityFolders(
  projectId: string | undefined,
  kind: EntityFolderKind,
): UseQueryResult<EntityFolder[]> {
  return useEntityFolderData(projectId, kind, foldersOf);
}

/** The entities the caller pinned in one list: their personal Pinned group, private to them. */
export function useEntityPins(projectId: string | undefined, kind: EntityFolderKind): UseQueryResult<readonly number[]> {
  return useEntityFolderData(projectId, kind, pinnedIdsOf);
}

interface ReorderInput {
  readonly folderId: string;
  readonly above: string | null;
  readonly below: string | null;
}

interface MoveInput {
  readonly entityId: number;
  /** The target folder, or null for the root. */
  readonly folderId: string | null;
}

interface PinInput {
  readonly entityId: number;
  readonly pinned: boolean;
}

interface EntityFolderMutations {
  readonly create: UseMutationResult<unknown, Error, string>;
  readonly rename: UseMutationResult<unknown, Error, { readonly folderId: string; readonly name: string }>;
  readonly remove: UseMutationResult<unknown, Error, string>;
  readonly reorder: UseMutationResult<unknown, Error, ReorderInput>;
  readonly move: UseMutationResult<unknown, Error, MoveInput>;
  readonly pin: UseMutationResult<unknown, Error, PinInput>;
  /** One move with no refetch: the caller makes several and calls `refreshEntities` once. It lands at the top of the folder. */
  readonly moveQuiet: (entityId: number, folderId: string | null) => Promise<unknown>;
  /** Refetches the folders and the entity lists, after one or several quiet moves. */
  readonly refreshEntities: () => void;
  /** Moves each id in turn (there is no batch endpoint), refetches once, and says which ones the server refused. */
  readonly moveMany: (entityIds: readonly number[], folderId: string | null) => Promise<{ readonly failed: readonly number[] }>;
}

/**
 * The write side. Every success refetches the folder list; a move or a delete
 * also refetches the agents/pipelines list, which the server narrows by
 * `folder_id`, so a card that left the open folder leaves the screen.
 */
export function useEntityFolderMutations(projectId: string | undefined, kind: EntityFolderKind): EntityFolderMutations {
  const queryClient = useQueryClient();
  const project = projectId ?? '';
  const refresh = (entitiesChanged: boolean): void => {
    void queryClient.invalidateQueries({ queryKey: [...ROOT, projectId, kind] });
    if (!entitiesChanged) return;
    void queryClient.invalidateQueries({
      predicate: (query) => {
        const head = query.queryKey[0];
        return typeof head === 'string' && head.startsWith(`/elitea_core/applications/prompt_lib/${project}`);
      },
    });
  };
  const create = useMutation<unknown, Error, string>({
    mutationFn: (name) => createEntityFolder(project, kind, { name }),
    onSuccess: () => refresh(false),
  });
  const rename = useMutation<unknown, Error, { readonly folderId: string; readonly name: string }>({
    mutationFn: ({ folderId, name }) => updateEntityFolder(project, kind, folderId, { name }),
    onSuccess: () => refresh(false),
  });
  const remove = useMutation<unknown, Error, string>({
    mutationFn: (folderId) => deleteEntityFolder(project, kind, folderId),
    onSuccess: () => refresh(true),
  });
  const reorder = useMutation<unknown, Error, ReorderInput>({
    mutationFn: ({ folderId, above, below }) =>
      updateEntityFolder(project, kind, folderId, { neighbor_above_id: above, neighbor_below_id: below }),
    onSuccess: () => refresh(false),
  });
  const move = useMutation<unknown, Error, MoveInput>({
    mutationFn: ({ entityId, folderId }) => moveEntityToFolder(project, kind, entityId, { folder_id: folderId }),
    onSuccess: () => refresh(true),
  });
  const pin = useMutation<unknown, Error, PinInput>({
    mutationFn: ({ entityId, pinned }) => setEntityPin(project, kind, entityId, { pinned }),
    onSuccess: () => refresh(true),
  });
  const moveQuiet = (entityId: number, folderId: string | null): Promise<unknown> =>
    moveEntityToFolder(project, kind, entityId, { folder_id: folderId });
  const refreshEntities = (): void => {
    refresh(true);
  };
  const moveMany = async (entityIds: readonly number[], folderId: string | null): Promise<{ readonly failed: readonly number[] }> => {
    const failed: number[] = [];
    for (const entityId of entityIds) {
      try {
        // eslint-disable-next-line no-await-in-loop -- sequential by design: there is no batch endpoint and each refusal is reported on its own.
        await moveQuiet(entityId, folderId);
      } catch {
        failed.push(entityId);
      }
    }
    refresh(true);
    return { failed };
  };
  return { create, rename, remove, reorder, move, pin, moveQuiet, refreshEntities, moveMany };
}
