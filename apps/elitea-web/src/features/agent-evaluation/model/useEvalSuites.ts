/**
 * React-query bindings for suites (run limits) and human scores.
 *
 * A human score changes three things a reader sees: the score list, the
 * scorecard (the result turns `ok`) and the runs list (the headline moves), so
 * a recorded score invalidates the whole `evalRuns` namespace, which
 * `useEvalRuns` already roots every run key under.
 */
import { useMutation, useQuery, useQueryClient, type UseQueryResult } from '@tanstack/react-query';

import type { EvalHumanScore, EvalHumanScoreWriteRequest, EvalSuite, EvalSuiteWriteRequest } from '@/shared/api/generated/model';

import {
  createSuite,
  fetchEvalSuites,
  fetchHumanScores,
  recordHumanScore,
  removeSuite,
  saveSuite,
} from '../api/evaluationSuitesApi';
import { evalRunQueryKeys } from './useEvalRuns';

const suiteKeys = {
  all: (projectId: string) => ['evalSuites', projectId] as const,
};

const humanScoreKeys = {
  list: (projectId: string, runId: string) => ['evalRuns', projectId, 'humanScores', runId] as const,
};

function enabled(projectId: string | undefined): boolean {
  return projectId !== undefined && projectId !== '';
}

export function useEvalSuites(projectId: string | undefined): UseQueryResult<EvalSuite[]> {
  return useQuery<EvalSuite[]>({
    queryKey: suiteKeys.all(projectId ?? ''),
    queryFn: () => fetchEvalSuites(projectId ?? ''),
    enabled: enabled(projectId),
  });
}

interface SaveSuiteArgs {
  /** Absent creates a suite; present replaces that one. */
  readonly suiteId: string | undefined;
  readonly input: EvalSuiteWriteRequest;
}

export function useEvalSuiteMutations(projectId: string | undefined) {
  const queryClient = useQueryClient();
  const invalidate = async (): Promise<void> => {
    if (!enabled(projectId)) return;
    await queryClient.invalidateQueries({ queryKey: suiteKeys.all(projectId ?? '') });
  };
  return {
    save: useMutation({
      mutationFn: (args: SaveSuiteArgs) =>
        args.suiteId === undefined
          ? createSuite(projectId ?? '', args.input)
          : saveSuite(projectId ?? '', args.suiteId, args.input),
      onSuccess: invalidate,
    }),
    remove: useMutation({
      mutationFn: (suiteId: string) => removeSuite(projectId ?? '', suiteId),
      onSuccess: invalidate,
    }),
  };
}

export function useEvalHumanScores(
  projectId: string | undefined,
  runId: string | undefined,
): UseQueryResult<EvalHumanScore[]> {
  return useQuery<EvalHumanScore[]>({
    queryKey: humanScoreKeys.list(projectId ?? '', runId ?? ''),
    queryFn: () => fetchHumanScores(projectId ?? '', runId ?? ''),
    enabled: enabled(projectId) && runId !== undefined && runId !== '',
  });
}

export function useRecordHumanScore(projectId: string | undefined, runId: string | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: EvalHumanScoreWriteRequest) =>
      recordHumanScore(projectId ?? '', runId ?? '', input),
    onSuccess: async () => {
      if (!enabled(projectId)) return;
      await queryClient.invalidateQueries({ queryKey: evalRunQueryKeys.runs(projectId ?? '') });
    },
  });
}
