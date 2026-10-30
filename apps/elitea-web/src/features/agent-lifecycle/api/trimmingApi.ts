import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  listProjectPermissionTrimming,
  removeProjectPermissionTrimmingItem,
} from '@/shared/api/generated/applications/applications';
import type {
  PermissionTrimmingRemoveRequest,
  PermissionTrimmingRemoveResult,
  PermissionTrimmingReport,
} from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

import { lifecycleErrorMessage } from '../model/useEntityLifecycle';

/**
 * Permission trimming (ADR-0044 decision 5, #1501): the agent's granted
 * tools, toolkits and credential use grants against their use in the step
 * records of the window, from the project route (`agents.trimming.manage`).
 * A removal goes through the same mutation path as the editor and is audited.
 */

const KEY = ['agent-lifecycle', 'permission-trimming'] as const;

export function useAgentPermissionTrimming(
  projectId: string | undefined,
  applicationId: number | undefined,
  enabled: boolean,
) {
  return useQuery({
    queryKey: [...KEY, projectId, applicationId],
    enabled: enabled && projectId !== undefined && projectId !== '' && applicationId !== undefined,
    queryFn: async () =>
      unwrapBody(
        await listProjectPermissionTrimming(projectId ?? '', {
          application_id: applicationId ?? 0,
        }),
      ) as PermissionTrimmingReport,
  });
}

export function useRemoveAgentPermission(projectId: string | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: PermissionTrimmingRemoveRequest) =>
      unwrapBody(await removeProjectPermissionTrimmingItem(projectId ?? '', body)) as PermissionTrimmingRemoveResult,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: [...KEY, projectId] }),
  });
}

/**
 * The sentence of a credential review refusal: its `message` (the server's
 * own sentence, e.g. why an item is not removable), else the lifecycle
 * fallback for a transport failure or an unrecognised body.
 */
export function credentialReviewErrorMessage(error: unknown): string {
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    const body = error.failure.body;
    if (typeof body === 'object' && body !== null) {
      const message = (body as Record<string, unknown>)['message'];
      if (typeof message === 'string' && message !== '') return message;
    }
  }
  return lifecycleErrorMessage(error);
}
