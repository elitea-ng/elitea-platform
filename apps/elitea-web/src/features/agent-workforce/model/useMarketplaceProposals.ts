/**
 * The Marketplace proposals of workforce managers (ADR-0051 decision 6): the
 * hire_listing and feedback_report proposals the agent manager decides. A hire
 * settles through the hire wizard (the hire request carries the proposal id),
 * so its queries are invalidated by the caller; a report is published or
 * discarded here.
 */
import { useCallback } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { getListAgentWorkforceProposalsQueryKey } from '@/shared/api/generated/applications/applications';
import type { AgentWorkforceProposal, ListAgentWorkforceProposals200 } from '@/shared/api/generated/model';

import { decide, fetchMarketplaceProposals, publishWorkforceReport } from '../api/agentWorkforceApi';

export function useMarketplaceProposals(projectId: string, enabled: boolean) {
  return useQuery<ListAgentWorkforceProposals200>({
    queryKey: [...getListAgentWorkforceProposalsQueryKey(projectId), 'marketplace'],
    queryFn: () => fetchMarketplaceProposals(projectId),
    enabled: enabled && projectId !== '',
    retry: false,
  });
}

export function useMarketplaceProposalActions(projectId: string) {
  const client = useQueryClient();
  const invalidate = useCallback(
    () => client.invalidateQueries({ queryKey: getListAgentWorkforceProposalsQueryKey(projectId) }),
    [client, projectId],
  );
  const discard = useMutation<AgentWorkforceProposal, Error, number>({
    mutationFn: (proposalId) => decide(projectId, proposalId, 'reject', {}),
    onSuccess: invalidate,
  });
  const publish = useMutation<void, Error, { readonly proposal: AgentWorkforceProposal; readonly text: string | undefined }>({
    mutationFn: ({ proposal, text }) => publishWorkforceReport(projectId, proposal.listing_id ?? 0, proposal.id, text),
    onSuccess: invalidate,
  });
  return { discard, publish, invalidate };
}
