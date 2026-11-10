/**
 * The queries and writes over one agent's workforce state (ADR-0050
 * decisions 5b and 5g-5m). Every write invalidates the agent's workforce
 * reads AND the project's proposal list, because a decision moves both: an
 * approval changes the agent's state, a proposal adds a row.
 */
import { useCallback } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  getGetAgentWorkforceQueryKey,
  getListAgentWorkforceProposalsQueryKey,
} from '@/shared/api/generated/applications/applications';
import type {
  AgentWorkforceAgent,
  AgentWorkforceImpact,
  AgentWorkforceProposal,
  ListAgentScorecards200,
  ListAgentWorkforceProposals200,
  ProposeAgentWorkforceStageBody,
} from '@/shared/api/generated/model';

import {
  appeal,
  comment,
  decide,
  decideAppeal,
  fetchImpact,
  fetchProposals,
  fetchScorecards,
  fetchWorkforceAgent,
  propose,
  reinstate,
  type WorkforceDecision,
} from '../api/agentWorkforceApi';

/** The trend the panel shows: the last 30 daily scorecards. */
const SCORECARD_DAYS = 30;
const PROPOSAL_PAGE_SIZE = 20;

function enabledFor(projectId: string | undefined, applicationId: number | undefined): boolean {
  return projectId !== undefined && projectId !== '' && applicationId !== undefined && applicationId > 0;
}

function agentKey(projectId: string, applicationId: number): readonly unknown[] {
  return getGetAgentWorkforceQueryKey(projectId, applicationId);
}

export function useWorkforceAgent(projectId: string | undefined, applicationId: number | undefined) {
  return useQuery<AgentWorkforceAgent>({
    queryKey: agentKey(projectId ?? '', applicationId ?? 0),
    queryFn: () => fetchWorkforceAgent(projectId ?? '', applicationId ?? 0),
    enabled: enabledFor(projectId, applicationId),
    retry: false,
  });
}

/** The 30-day trend; only an agent manager or the owner may read it (ADR-0050 5h). */
export function useScorecards(projectId: string | undefined, applicationId: number | undefined, allowed: boolean) {
  return useQuery<ListAgentScorecards200>({
    queryKey: [...agentKey(projectId ?? '', applicationId ?? 0), 'scorecards', SCORECARD_DAYS],
    queryFn: () => fetchScorecards(projectId ?? '', applicationId ?? 0, SCORECARD_DAYS),
    enabled: allowed && enabledFor(projectId, applicationId),
    retry: false,
  });
}

/** The impact preview, read only while a decision dialog is open. */
export function useImpact(projectId: string | undefined, applicationId: number | undefined, enabled: boolean) {
  return useQuery<AgentWorkforceImpact>({
    queryKey: [...agentKey(projectId ?? '', applicationId ?? 0), 'impact'],
    queryFn: () => fetchImpact(projectId ?? '', applicationId ?? 0),
    enabled: enabled && enabledFor(projectId, applicationId),
    retry: false,
    staleTime: 0,
  });
}

/** Every proposal for this agent, newest first; an agent manager or the owner reads them. */
export function useAgentProposals(projectId: string | undefined, applicationId: number | undefined, allowed: boolean) {
  return useQuery<ListAgentWorkforceProposals200>({
    queryKey: [...getListAgentWorkforceProposalsQueryKey(projectId ?? ''), 'agent', applicationId],
    queryFn: () => fetchProposals(projectId ?? '', { status: 'all', application_id: applicationId ?? 0, limit: PROPOSAL_PAGE_SIZE }),
    enabled: allowed && enabledFor(projectId, applicationId),
    retry: false,
  });
}

/** The writes. Each answers the server's row; errors surface to the caller. */
export function useWorkforceActions(projectId: string | undefined, applicationId: number | undefined) {
  const client = useQueryClient();
  const project = projectId ?? '';
  const agent = applicationId ?? 0;
  const invalidate = useCallback(async () => {
    await Promise.all([
      client.invalidateQueries({ queryKey: agentKey(project, agent) }),
      client.invalidateQueries({ queryKey: getListAgentWorkforceProposalsQueryKey(project) }),
    ]);
  }, [client, project, agent]);

  const proposeMutation = useMutation<AgentWorkforceProposal, Error, Omit<ProposeAgentWorkforceStageBody, 'application_id'>>({
    mutationFn: (body) => propose(project, { ...body, application_id: agent }),
    onSuccess: invalidate,
  });
  const decideMutation = useMutation<
    AgentWorkforceProposal,
    Error,
    { readonly proposalId: number; readonly decision: WorkforceDecision; readonly note: string; readonly confirmNested: boolean }
  >({
    mutationFn: ({ proposalId, decision, note, confirmNested }) =>
      decide(project, proposalId, decision, { ...(note === '' ? {} : { note }), ...(confirmNested ? { confirm_nested: true } : {}) }),
    onSuccess: invalidate,
  });
  const ownerTextMutation = useMutation<AgentWorkforceProposal, Error, { readonly proposalId: number; readonly kind: 'comment' | 'appeal'; readonly text: string }>({
    mutationFn: ({ proposalId, kind, text }) => (kind === 'comment' ? comment(project, proposalId, text) : appeal(project, proposalId, text)),
    onSuccess: invalidate,
  });
  const appealDecisionMutation = useMutation<AgentWorkforceProposal, Error, { readonly proposalId: number; readonly grant: boolean; readonly note: string }>({
    mutationFn: ({ proposalId, grant, note }) => decideAppeal(project, proposalId, grant, note),
    onSuccess: invalidate,
  });
  const reinstateMutation = useMutation<AgentWorkforceAgent, Error, string>({
    mutationFn: (note) => reinstate(project, agent, note),
    onSuccess: invalidate,
  });
  return {
    propose: proposeMutation,
    decide: decideMutation,
    ownerText: ownerTextMutation,
    decideAppeal: appealDecisionMutation,
    reinstate: reinstateMutation,
  };
}
