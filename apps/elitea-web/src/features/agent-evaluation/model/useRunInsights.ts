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

import { fetchPreviousScoredRun, fetchRunAnalytics } from '../api/evaluationInsightsApi';
import { fetchScorecard } from '../api/evaluationRunsApi';
import { evalRunQueryKeys } from './useEvalRuns';

async function previousOrNull(projectId: string, run: EvalRun | undefined): Promise<EvalRun | null> {
  return run === undefined ? null : ((await fetchPreviousScoredRun(projectId, run)) ?? null);
}

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

function usePreviousScoredRun(projectId: string | undefined, run: EvalRun | undefined, enabled: boolean) {
  return useQuery<EvalRun | null>({
    queryKey: ['evalRuns', projectId ?? '', 'previous', run?.id ?? ''],
    // null, not undefined: react-query refuses an undefined result.
    queryFn: () => previousOrNull(projectId ?? '', run),
    enabled: enabled && present(projectId, run?.id),
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
  const history = usePreviousScoredRun(projectId, run, enabled);
  const previous = history.data ?? undefined;
  const scorecard = useQuery<EvalScorecard>({
    queryKey: evalRunQueryKeys.scorecard(projectId ?? '', previous?.id ?? ''),
    queryFn: () => fetchScorecard(projectId ?? '', previous?.id ?? ''),
    enabled: enabled && present(projectId, previous?.id),
  });
  return { previous, scorecard, loading: history.isPending && enabled };
}
