/**
 * The project's workforce policy and its automatic actions (ADR-0050
 * decisions 5c and 5k, issue 1617). Every write invalidates the policy, the
 * action list and the workforce reads: undoing a hire retires an agent, and
 * an objection rejects a proposal.
 */
import { useCallback } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  getGetAgentWorkforcePolicyQueryKey,
  getListAgentAutomaticActionsQueryKey,
  getListAgentWorkforceProposalsQueryKey,
} from '@/shared/api/generated/applications/applications';
import type {
  AgentAutomaticAction,
  AgentWorkforcePolicy,
  AgentWorkforcePolicyInput,
  ListAgentAutomaticActions200,
} from '@/shared/api/generated/model';

import {
  decideAutomaticAction,
  fetchAutomaticActions,
  fetchPolicy,
  savePolicy,
  type AutomaticActionDecision,
} from '../api/agentWorkforceApi';

const ACTION_PAGE_SIZE = 50;

export function useWorkforcePolicy(projectId: string) {
  return useQuery<AgentWorkforcePolicy>({
    queryKey: getGetAgentWorkforcePolicyQueryKey(projectId),
    queryFn: () => fetchPolicy(projectId),
    enabled: projectId !== '',
    retry: false,
  });
}

export function useAutomaticActions(projectId: string) {
  return useQuery<ListAgentAutomaticActions200>({
    queryKey: getListAgentAutomaticActionsQueryKey(projectId),
    queryFn: () => fetchAutomaticActions(projectId, { limit: ACTION_PAGE_SIZE }),
    enabled: projectId !== '',
    retry: false,
  });
}

export function useWorkforcePolicyActions(projectId: string) {
  const client = useQueryClient();
  const invalidate = useCallback(async () => {
    await Promise.all([
      client.invalidateQueries({ queryKey: getGetAgentWorkforcePolicyQueryKey(projectId) }),
      client.invalidateQueries({ queryKey: getListAgentAutomaticActionsQueryKey(projectId) }),
      client.invalidateQueries({ queryKey: getListAgentWorkforceProposalsQueryKey(projectId) }),
    ]);
  }, [client, projectId]);
  const save = useMutation<AgentWorkforcePolicy, Error, AgentWorkforcePolicyInput>({
    mutationFn: (body) => savePolicy(projectId, body),
    onSuccess: invalidate,
  });
  const decide = useMutation<
    AgentAutomaticAction,
    Error,
    { readonly actionId: number; readonly decision: AutomaticActionDecision; readonly confirmNested: boolean }
  >({
    mutationFn: ({ actionId, decision, confirmNested }) =>
      decideAutomaticAction(projectId, actionId, decision, confirmNested ? { confirm_nested: true } : {}),
    onSuccess: invalidate,
  });
  return { save, decide };
}
