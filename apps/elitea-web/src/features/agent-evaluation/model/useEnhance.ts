/** Mutations behind the Enhance with AI wizard. */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import type { EvalEnhanceProposal } from '@/shared/api/generated/model';

import { applyEnhancement, fetchVersionInstructions, requestEnhancement, type Acceptance } from '../api/evaluationInsightsApi';
import type { EvalDimension } from './types';

export function useEnhanceRun(projectId: string | undefined) {
  return useMutation({
    mutationFn: (runId: string) => requestEnhancement(projectId ?? '', runId),
  });
}

interface ApplyArgs {
  readonly proposal: EvalEnhanceProposal;
  readonly accepted: Acceptance;
  readonly library: readonly EvalDimension[];
}

export function useApplyEnhancement(projectId: string | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (args: ApplyArgs) => applyEnhancement(projectId ?? '', args.proposal, args.accepted, args.library),
    // The writes touch dimensions, datasets and the agent's versions, so every
    // namespace the evaluation tab reads is refreshed.
    onSettled: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['evalDimensions', projectId ?? ''] }),
        queryClient.invalidateQueries({ queryKey: ['evalDatasets', projectId ?? ''] }),
      ]);
    },
  });
}

function hasPatch(proposal: EvalEnhanceProposal | undefined): boolean {
  return proposal?.instruction_patch !== null && proposal?.instruction_patch !== undefined;
}

function isId(value: number | null | undefined): value is number {
  return value !== null && value !== undefined;
}

export function useVersionInstructions(projectId: string | undefined, proposal: EvalEnhanceProposal | undefined) {
  const applicationId = proposal?.application_id;
  const versionId = proposal?.application_version_id;
  return useQuery({
    queryKey: ['evalRuns', projectId ?? '', 'versionInstructions', applicationId, versionId],
    queryFn: () => fetchVersionInstructions(projectId ?? '', applicationId ?? 0, versionId ?? 0),
    enabled: projectId !== undefined && projectId !== '' && isId(applicationId) && isId(versionId) && hasPatch(proposal),
    retry: false,
  });
}
