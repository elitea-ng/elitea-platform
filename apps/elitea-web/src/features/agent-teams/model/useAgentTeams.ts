/**
 * The queries and writes over a project's saved teams (issue 1620).
 *
 * Every team write invalidates every cached read of the project's teams by
 * the list route's key. Starting a conversation from a team also refreshes
 * the conversation lists, so the new conversation is in the chat rail when
 * the chat opens it.
 */
import { useMemo } from 'react';

import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { usePermissionList } from '@/shared/api/generated/auth/auth';
import { getListAgentTeamsQueryKey, getListConversationsQueryKey } from '@/shared/api/generated/chat/chat';
import type { AgentTeam, AgentTeamList, AgentTeamWrite, CreatedConversation, Permission } from '@/shared/api/generated/model';

import {
  fetchAgentTeams,
  removeAgentTeam,
  replaceAgentTeam,
  saveAgentTeam,
  saveConversationAsTeam,
  startTeamConversation,
} from '../api/agentTeamsApi';

export const TEAM_PERMISSIONS = {
  list: 'models.teams.list',
  create: 'models.teams.create',
  update: 'models.teams.update',
  delete: 'models.teams.delete',
} as const;

function teamsKey(projectId: string): readonly unknown[] {
  return getListAgentTeamsQueryKey(projectId);
}

/** One page of the project's teams. */
export function useAgentTeamList(projectId: string | undefined, query: string, enabled = true) {
  const params = { query: query.trim() === '' ? undefined : query.trim(), limit: 200 };
  return useQuery<AgentTeamList>({
    queryKey: [...teamsKey(projectId ?? ''), params],
    queryFn: () => fetchAgentTeams(projectId ?? '', params),
    enabled: enabled && projectId !== undefined && projectId !== '',
    placeholderData: keepPreviousData,
    retry: false,
  });
}

/** What the caller may do with teams in this project. */
export function useTeamAbilities(projectId: string | undefined) {
  const query = usePermissionList(projectId ?? '', { query: { enabled: projectId !== undefined } });
  return useMemo(() => {
    const list = query.data?.data as Permission[] | undefined;
    const granted = new Set((list ?? []).filter((entry) => entry.enabled).map((entry) => entry.name));
    return {
      canList: granted.has(TEAM_PERMISSIONS.list),
      canCreate: granted.has(TEAM_PERMISSIONS.create),
      canUpdate: granted.has(TEAM_PERMISSIONS.update),
      canDelete: granted.has(TEAM_PERMISSIONS.delete),
    };
  }, [query.data]);
}

export function useAgentTeamWrites(projectId: string) {
  const queryClient = useQueryClient();
  const invalidate = () => queryClient.invalidateQueries({ queryKey: teamsKey(projectId) });
  const create = useMutation<AgentTeam, unknown, AgentTeamWrite>({
    mutationFn: (team) => saveAgentTeam(projectId, team),
    onSuccess: () => void invalidate(),
  });
  const update = useMutation<AgentTeam, unknown, { readonly teamId: number; readonly team: AgentTeamWrite }>({
    mutationFn: ({ teamId, team }) => replaceAgentTeam(projectId, teamId, team),
    onSuccess: () => void invalidate(),
  });
  const remove = useMutation<void, unknown, number>({
    mutationFn: (teamId) => removeAgentTeam(projectId, teamId),
    onSuccess: () => void invalidate(),
  });
  const fromConversation = useMutation<
    AgentTeam,
    unknown,
    { readonly conversationId: string; readonly name: string; readonly description: string }
  >({
    mutationFn: ({ conversationId, name, description }) =>
      saveConversationAsTeam(projectId, conversationId, { name: name.trim(), description: description.trim() }),
    onSuccess: () => void invalidate(),
  });
  const start = useMutation<CreatedConversation, unknown, number>({
    mutationFn: (teamId) => startTeamConversation(projectId, teamId, {}),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: getListConversationsQueryKey(projectId).slice(0, 1) }),
  });
  return { create, update, remove, fromConversation, start };
}
