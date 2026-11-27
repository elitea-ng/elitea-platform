/**
 * The platform's run caps at the agent's risk tier (issue 3137,
 * `getAgentRunCaps`): what the Run caps fields may not exceed. Optional by
 * design — without the read permission (403), on a deployment with no
 * database (503) or before the route exists, the fields keep the contract's
 * own limits and the save's 422 RUN_CAP_ABOVE_TIER names the cap.
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { getAgentRunCaps, getGetAgentRunCapsQueryKey } from '@/shared/api/generated/settings/settings';
import type { AgentRunCaps } from '@/shared/api/generated/model';

/* manifest: settings.getAgentRunCaps */
async function fetchTierRunCaps(projectId: string, applicationId: number): Promise<AgentRunCaps> {
  return ((await getAgentRunCaps(projectId, applicationId)) as unknown as { readonly data: AgentRunCaps }).data;
}

export function useTierRunCaps(projectId: string | undefined, applicationId: number | undefined): UseQueryResult<AgentRunCaps> {
  const project = projectId ?? '';
  const application = applicationId ?? 0;
  return useQuery<AgentRunCaps>({
    queryKey: getGetAgentRunCapsQueryKey(project, application),
    queryFn: () => fetchTierRunCaps(project, application),
    enabled: project !== '',
    retry: false,
    staleTime: 60_000,
  });
}
