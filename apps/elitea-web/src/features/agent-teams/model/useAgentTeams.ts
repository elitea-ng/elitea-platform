/**
 * The queries and writes over a project's saved teams (issue 1620).
 *
 * Every team write invalidates every cached read of the project's teams by
 * the list route's key. Starting a conversation from a team also refreshes
 * the conversation lists, so the new conversation is in the chat rail when
 * the chat opens it.
 */
import { useMemo } from 'react';

import { keepPreviousData, useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';

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

/** The route's largest page (`limit` max in the contract). */
const TEAM_PAGE_SIZE = 200;

/** Where the next page starts, or undefined once every team is loaded. */
function nextTeamOffset(pages: readonly AgentTeamList[]): number | undefined {
  const loaded = pages.reduce((sum, page) => sum + page.rows.length, 0);
  const last = pages.at(-1);
  if (last === undefined || last.rows.length === 0 || loaded >= last.total) return undefined;
  return loaded;
}

/**
 * Every team the pages hold, once each. Paging is by offset over a list
 * sorted by name, so a team created or deleted between pages shifts the
 * window and a team can come back on the next page.
 */
function uniqueTeams(pages: readonly AgentTeamList[]): AgentTeam[] {
  const seen = new Set<number>();
  return pages.flatMap((page) => page.rows).filter((team) => (seen.has(team.id) ? false : (seen.add(team.id), true)));
}

/**
 * The project's teams, a page at a time (#1847): `rows` is every team loaded
 * so far, `total` the project's count, and `loadMore` fetches the next page.
 */
export function useAgentTeamList(projectId: string | undefined, query: string, enabled = true) {
  const params = { query: query.trim() === '' ? undefined : query.trim(), limit: TEAM_PAGE_SIZE };
  const list = useInfiniteQuery({
    queryKey: [...teamsKey(projectId ?? ''), params, 'pages'],
    queryFn: ({ pageParam }) => fetchAgentTeams(projectId ?? '', { ...params, offset: pageParam }),
    initialPageParam: 0,
    getNextPageParam: (_last, pages) => nextTeamOffset(pages),
    enabled: enabled && projectId !== undefined && projectId !== '',
    placeholderData: keepPreviousData,
    retry: false,
  });
  const pages = list.data?.pages ?? [];
  return {
    rows: uniqueTeams(pages),
    total: pages.at(-1)?.total ?? 0,
    isSuccess: list.isSuccess,
    isError: list.isError,
    error: list.error,
    hasMore: list.hasNextPage,
    loadingMore: list.isFetchingNextPage,
    loadMore: () => void list.fetchNextPage(),
  };
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
