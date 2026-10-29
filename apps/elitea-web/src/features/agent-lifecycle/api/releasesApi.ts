import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  getApplicationRelease,
  listApplicationReleases,
  listVersionRevisions,
  rollbackApplicationRelease,
} from '@/shared/api/generated/applications/applications';
import type {
  ApplicationReleaseDetail,
  ApplicationReleaseList,
  ApplicationReleaseRollback,
  VersionRevisionList,
} from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

/**
 * Agent releases and draft revisions (ADR-0040 decision 2, #1492), as the
 * generated client serves them. Each call reads through the envelope:
 * `eliteaFetch` resolves `{data, status, headers}`, and the generated
 * `Promise<T>` is a cast over it.
 */

const KEY = ['agent-lifecycle', 'releases'] as const;

function rowsBody<T extends { rows: unknown[] }>(response: unknown, what: string): T {
  const body = unwrapBody(response);
  if (typeof body === 'object' && body !== null && Array.isArray((body as { rows?: unknown }).rows)) {
    return body as T;
  }
  throw new Error(`${what}: the server answered with an unrecognised body`);
}

/** Every release of the agent, newest first. */
export function useApplicationReleases(projectId: string | undefined, applicationId: number | undefined, enabled: boolean) {
  return useQuery({
    queryKey: [...KEY, projectId, applicationId],
    enabled: enabled && projectId !== undefined && applicationId !== undefined,
    queryFn: async () =>
      rowsBody<ApplicationReleaseList>(
        await listApplicationReleases(projectId ?? '', applicationId ?? 0),
        'agent releases',
      ),
  });
}

/** One release with its changes against the release before it. */
export function useApplicationRelease(
  projectId: string | undefined,
  applicationId: number | undefined,
  releaseId: number | undefined,
) {
  return useQuery({
    queryKey: [...KEY, projectId, applicationId, 'detail', releaseId],
    enabled: projectId !== undefined && applicationId !== undefined && releaseId !== undefined,
    queryFn: async () =>
      unwrapBody(
        await getApplicationRelease(projectId ?? '', applicationId ?? 0, releaseId ?? 0),
      ) as ApplicationReleaseDetail,
  });
}

/** The draft revisions of one version, newest first. */
export function useVersionRevisions(
  projectId: string | undefined,
  applicationId: number | undefined,
  versionId: number | undefined,
  enabled: boolean,
) {
  return useQuery({
    queryKey: [...KEY, projectId, applicationId, 'revisions', versionId],
    enabled: enabled && projectId !== undefined && applicationId !== undefined && versionId !== undefined,
    queryFn: async () =>
      rowsBody<VersionRevisionList>(
        await listVersionRevisions(projectId ?? '', applicationId ?? 0, versionId ?? 0),
        'version revisions',
      ),
  });
}

/** Re-points the published release to `releaseId`. */
export function useRollbackRelease(projectId: string | undefined, applicationId: number | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ releaseId, reason }: { releaseId: number; reason: string }) =>
      unwrapBody(
        await rollbackApplicationRelease(projectId ?? '', applicationId ?? 0, releaseId, reason === '' ? {} : { reason }),
      ) as ApplicationReleaseRollback,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: [...KEY, projectId, applicationId] });
    },
  });
}
