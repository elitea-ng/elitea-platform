/**
 * The queries and writes over a project's agent proposals (ADR-0050
 * decision 5a). Every write invalidates every cached proposal read of the
 * project by the list route's key prefix, which the single-proposal key
 * shares, so the inbox and a chat card never disagree.
 */
import { useCallback } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { getListAgentProposalsQueryKey } from '@/shared/api/generated/applications/applications';
import type { AgentProposal, AgentProposalList } from '@/shared/api/generated/model';

import {
  createProposedAgent,
  dismissProposal,
  fetchAgentProposal,
  fetchAgentProposals,
  markProposalCreated,
} from '../api/agentProposalsApi';
import { AgentMadeError } from '../lib/proposalErrors';
import { createRequestOf, type ReviewDraft } from '../lib/proposalDraft';

/** The inbox shows at most this many open proposals at once (3 per proposing agent). */
const INBOX_PAGE_SIZE = 50;

function proposalsKey(projectId: string): readonly unknown[] {
  return getListAgentProposalsQueryKey(projectId);
}

function enabledFor(projectId: string | undefined): projectId is string {
  return projectId !== undefined && projectId !== '';
}

/** The project's open proposals, newest first. */
export function useOpenProposals(projectId: string | undefined, enabled = true) {
  return useQuery<AgentProposalList>({
    queryKey: [...proposalsKey(projectId ?? ''), 'open'],
    queryFn: () => fetchAgentProposals(projectId ?? '', { status: 'proposed', limit: INBOX_PAGE_SIZE }),
    enabled: enabled && enabledFor(projectId),
    retry: false,
  });
}

/** One proposal as it is now (it may have been decided elsewhere). */
export function useProposal(projectId: string | undefined, proposalId: number | undefined) {
  return useQuery<AgentProposal>({
    queryKey: [...proposalsKey(projectId ?? ''), 'one', proposalId],
    queryFn: () => fetchAgentProposal(projectId ?? '', proposalId ?? 0),
    enabled: enabledFor(projectId) && proposalId !== undefined && proposalId > 0,
    retry: false,
  });
}

interface CreateVariables {
  readonly proposal: AgentProposal;
  readonly review: ReviewDraft;
  /** Indexes into `proposal.memory_seeds` the person kept. */
  readonly seeds: readonly number[];
  /** The agent an earlier attempt already created (AgentMadeError): a retry only records it. */
  readonly existingApplicationId?: number | undefined;
}

export interface CreatedAgent {
  readonly applicationId: number;
  readonly memorySeeded: number;
}

/** Create (the ordinary create route, then the proposal records it) and Dismiss. */
export function useProposalDecisions(projectId: string) {
  const queryClient = useQueryClient();
  const invalidate = useCallback(() => queryClient.invalidateQueries({ queryKey: proposalsKey(projectId) }), [queryClient, projectId]);
  const create = useMutation<CreatedAgent, Error, CreateVariables>({
    mutationFn: async ({ proposal, review, seeds, existingApplicationId }) => {
      const applicationId = existingApplicationId ?? Number((await createProposedAgent(projectId, createRequestOf(review))).id);
      try {
        const marked = await markProposalCreated(projectId, proposal.id, applicationId, seeds);
        return { applicationId, memorySeeded: marked.memory_seeded };
      } catch (cause) {
        throw new AgentMadeError(applicationId, cause);
      }
    },
    onSettled: invalidate,
  });
  const dismiss = useMutation<AgentProposal, Error, number>({
    mutationFn: (proposalId) => dismissProposal(projectId, proposalId),
    onSettled: invalidate,
  });
  return { create, dismiss };
}
