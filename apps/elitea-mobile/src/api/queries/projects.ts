import { useQuery } from '@tanstack/react-query';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import { queryKeys } from '@/api/queries/keys';

/**
 * The caller's projects (`listProjects`). The contract mounts the list at
 * `/projects/project/default/{public_project_id}` and the live route uses the
 * literal `1` there (plan-app F2).
 */
export const PROJECT_LIST_SEGMENT = 1;

export interface Project {
  id: number;
  name: string;
  suspended: boolean;
}

export async function fetchProjects(
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<Project[]> {
  const rows = await unwrap(
    client.GET('/projects/project/default/{public_project_id}', {
      params: { path: { public_project_id: PROJECT_LIST_SEGMENT } },
      signal,
    }),
  );
  if (!Array.isArray(rows)) return [];
  return rows
    .filter((p) => Number.isSafeInteger(p.id) && p.id > 0)
    .map((p) => ({ id: p.id, name: typeof p.name === 'string' ? p.name : `Project ${p.id}`, suspended: p.suspended === true }));
}

/** Projects the user can switch to: the suspended ones are hidden. */
export function selectableProjects(projects: readonly Project[]): Project[] {
  return projects.filter((p) => !p.suspended);
}

/**
 * The project to use: `selectedId` when it is in the list and not suspended,
 * else the first non-suspended project, else null.
 */
export function resolveProjectSelection(projects: readonly Project[], selectedId: number | null): number | null {
  const selectable = selectableProjects(projects);
  if (selectedId !== null && selectable.some((p) => p.id === selectedId)) return selectedId;
  return selectable[0]?.id ?? null;
}

export function useProjects() {
  return useQuery({
    queryKey: queryKeys.projects(),
    queryFn: ({ signal }) => fetchProjects(sessionApiClient(), signal),
    staleTime: 5 * 60_000,
  });
}
