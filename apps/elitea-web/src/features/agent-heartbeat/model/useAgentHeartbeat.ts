/**
 * The reads and the save of one agent's heartbeat. A save refreshes the
 * settings and the history, because a save that turns the heartbeat on makes
 * it due at once.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { getGetAgentHeartbeatQueryKey, getListAgentHeartbeatsQueryKey } from '@/shared/api/generated/applications/applications';
import type { AgentHeartbeatList, AgentHeartbeatSave, AgentHeartbeatSettings } from '@/shared/api/generated/model';

import { fetchAgentHeartbeat, fetchAgentHeartbeats, storeAgentHeartbeat } from '../api/agentHeartbeatApi';

export const HEARTBEAT_HISTORY_LIMIT = 50;

export function useAgentHeartbeat(projectId: string, applicationId: number) {
  return useQuery<AgentHeartbeatSettings>({
    queryKey: getGetAgentHeartbeatQueryKey(projectId, applicationId),
    queryFn: () => fetchAgentHeartbeat(projectId, applicationId),
    retry: false,
  });
}

export function useAgentHeartbeatHistory(projectId: string, applicationId: number) {
  return useQuery<AgentHeartbeatList>({
    queryKey: getListAgentHeartbeatsQueryKey(projectId, applicationId, { limit: HEARTBEAT_HISTORY_LIMIT }),
    queryFn: () => fetchAgentHeartbeats(projectId, applicationId, HEARTBEAT_HISTORY_LIMIT),
    retry: false,
  });
}

export function useSaveAgentHeartbeat(projectId: string, applicationId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (save: AgentHeartbeatSave) => storeAgentHeartbeat(projectId, applicationId, save),
    onSuccess: (settings) => {
      queryClient.setQueryData(getGetAgentHeartbeatQueryKey(projectId, applicationId), settings);
      void queryClient.invalidateQueries({ queryKey: getListAgentHeartbeatsQueryKey(projectId, applicationId) });
    },
  });
}
