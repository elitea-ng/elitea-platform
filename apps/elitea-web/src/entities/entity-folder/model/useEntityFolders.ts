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
  updateEntityFolder,
} from '@/shared/api/generated/applications/applications';
import type { EntityFolder } from '@/shared/api/generated/model';
import { unwrapListPage } from '@/shared/api/unwrap';

import type { EntityFolderKind } from './kinds';

const ROOT = ['entity-folders'] as const;

/** The folders of one list, highest position first. Disabled until the project is known. */
export function useEntityFolders(
  projectId: string | undefined,
  kind: EntityFolderKind,
): UseQueryResult<EntityFolder[]> {
  return useQuery<EntityFolder[]>({
    queryKey: [...ROOT, projectId, kind],
    enabled: projectId !== undefined,
    queryFn: async ({ signal }) => {
      const response = await listEntityFolders(projectId ?? '', kind, { signal });
      return unwrapListPage<EntityFolder>(response, 'listEntityFolders').rows;
    },
  });
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

interface EntityFolderMutations {
  readonly create: UseMutationResult<unknown, Error, string>;
  readonly rename: UseMutationResult<unknown, Error, { readonly folderId: string; readonly name: string }>;
  readonly remove: UseMutationResult<unknown, Error, string>;
  readonly reorder: UseMutationResult<unknown, Error, ReorderInput>;
  readonly move: UseMutationResult<unknown, Error, MoveInput>;
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
  return { create, rename, remove, reorder, move };
}
