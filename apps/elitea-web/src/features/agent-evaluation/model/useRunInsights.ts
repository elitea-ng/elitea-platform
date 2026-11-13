/**
 * Queries behind a run's insights (issue 2011, step A).
 *
 * The analytics read is gated by its OWN permission (the analytics one, on top
 * of the run read), so a 403 here is the normal answer for a viewer who may
 * read runs and not spend. It is returned as a state, not retried and not
 * thrown into an error banner.
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import type { AnalyticsEvaluationRun, EvalRun, EvalScorecard } from '@/shared/api/generated/model';

import { fetchDatasetRuns, fetchRunAnalytics } from '../api/evaluationInsightsApi';
import { fetchScorecard } from '../api/evaluationRunsApi';
import { findPreviousRun } from '../lib/runInsights';
import { evalRunQueryKeys } from './useEvalRuns';

function present(...values: readonly (string | undefined)[]): boolean {
  return values.every((value) => value !== undefined && value !== '');
}

export function useRunAnalytics(
  projectId: string | undefined,
  runId: string | undefined,
  enabled: boolean,
): UseQueryResult<AnalyticsEvaluationRun> {
  return useQuery<AnalyticsEvaluationRun>({
    queryKey: ['evalRuns', projectId ?? '', 'analytics', runId ?? ''],
    queryFn: () => fetchRunAnalytics(projectId ?? '', runId ?? ''),
    enabled: enabled && present(projectId, runId),
    retry: false,
  });
}

/** The previous scored run of the same dataset, and its scorecard. */
export function usePreviousRun(
  projectId: string | undefined,
  run: EvalRun | undefined,
  enabled: boolean,
): {
  readonly previous: EvalRun | undefined;
  readonly scorecard: UseQueryResult<EvalScorecard>;
  readonly loading: boolean;
} {
  const history = useQuery<EvalRun[]>({
    queryKey: ['evalRuns', projectId ?? '', 'history', run?.dataset_id ?? ''],
    queryFn: () => fetchDatasetRuns(projectId ?? '', run?.dataset_id ?? '', run?.application_id ?? undefined),
    enabled: enabled && present(projectId, run?.dataset_id),
  });
  const previous = run === undefined ? undefined : findPreviousRun(history.data ?? [], run);
  const scorecard = useQuery<EvalScorecard>({
    queryKey: evalRunQueryKeys.scorecard(projectId ?? '', previous?.id ?? ''),
    queryFn: () => fetchScorecard(projectId ?? '', previous?.id ?? ''),
    enabled: enabled && present(projectId, previous?.id),
  });
  return { previous, scorecard, loading: history.isPending && enabled };
}
