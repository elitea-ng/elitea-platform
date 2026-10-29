import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  listAgentAttestations,
  recordAgentAttestation,
  revokeAgentAttestation,
} from '@/shared/api/generated/applications/applications';
import type { AgentAttestation, AgentAttestationCreate, AgentAttestationList } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

/**
 * Attestations (ADR-0040 decision 5): a review done outside the platform,
 * recorded against the agent, one of its releases, a toolkit or an index.
 * The governance dialog lists the agent's own; the route is project-wide and
 * filtered by subject.
 */

const KEY = ['agent-lifecycle', 'attestations'] as const;

export function useAgentAttestations(projectId: string | undefined, applicationId: number | undefined) {
  return useQuery({
    queryKey: [...KEY, projectId, applicationId],
    enabled: projectId !== undefined && projectId !== '' && applicationId !== undefined,
    queryFn: async () =>
      unwrapBody(
        await listAgentAttestations(projectId ?? '', { subject_type: 'agent', subject_id: String(applicationId ?? '') }),
      ) as AgentAttestationList,
  });
}

export function useRecordAttestation(projectId: string | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: AgentAttestationCreate) =>
      unwrapBody(await recordAgentAttestation(projectId ?? '', body)) as AgentAttestation,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: [...KEY, projectId] }),
  });
}

export function useRevokeAttestation(projectId: string | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ id, reason }: { readonly id: number; readonly reason: string }) =>
      unwrapBody(await revokeAgentAttestation(projectId ?? '', id, reason === '' ? {} : { reason })) as AgentAttestation,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: [...KEY, projectId] }),
  });
}
