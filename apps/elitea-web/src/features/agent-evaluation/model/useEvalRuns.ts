/**
 * React-query bindings for datasets, runs and the scorecard.
 *
 * ONE KEY NAMESPACE, built here and used by every reader and every
 * invalidation. Hand-built keys at the call site are how a mutation invalidates
 * one namespace while the list reads another: the request succeeds, the cache
 * is never refreshed, and the new row does not appear until a reload — a 200
 * that looks like a write that did nothing.
 *
 * HOW A RUNNING RUN IS FOLLOWED. By POLLING, not by a socket. The reference
 * follows a run over an `eval_run_progress` socket namespace; there is no
 * socket server in this platform (#126 deleted it, #615 recorded the decision),
 * and the reference's own hook already documents a polling fallback. `GET
 * eval_run` carries `progress{done,total}` and the status, so the poll stops
 * itself the moment the run reaches a terminal status — see `pollInterval`.
 */
import { useMutation, useQuery, useQueryClient, type UseQueryResult } from '@tanstack/react-query';

import type {
  EvalDataset,
  EvalDatasetCaseWriteRequest,
  EvalDatasetDetail,
  EvalDatasetWriteRequest,
  EvalRun,
  EvalRunStartRequest,
  EvalScorecard,
} from '@/shared/api/generated/model';

import {
  addCase,
  cancelRun,
  createDataset,
  fetchEvalDataset,
  fetchEvalDatasets,
  fetchEvalRun,
  fetchEvalRuns,
  fetchScorecard,
  removeCase,
  removeDataset,
  startRun,
} from '../api/evaluationRunsApi';

/**
 * A run is polled every three seconds while it is active. The interval is a
 * compromise stated rather than tuned: a run's unit of progress is one case,
 * each case is two model calls, so nothing moves faster than a few seconds and
 * a tighter poll would only add requests.
 */
const RUN_POLL_INTERVAL_MS = 3000;

/** The reference's `isRunTerminal`. A status this client does not know is NOT terminal. */
const TERMINAL_RUN_STATUSES = new Set(['finished', 'errored', 'cancelled']);

export function isRunTerminal(status: string | undefined): boolean {
  return status !== undefined && TERMINAL_RUN_STATUSES.has(status);
}

export const evalRunQueryKeys = {
  datasets: (projectId: string) => ['evalDatasets', projectId] as const,
  datasetList: (projectId: string, agentId: number | undefined) =>
    ['evalDatasets', projectId, 'list', agentId ?? 'project'] as const,
  dataset: (projectId: string, datasetId: string) =>
    ['evalDatasets', projectId, 'detail', datasetId] as const,
  runs: (projectId: string) => ['evalRuns', projectId] as const,
  runList: (projectId: string, agentId: number | undefined) =>
    ['evalRuns', projectId, 'list', agentId ?? 'project'] as const,
  run: (projectId: string, runId: string) => ['evalRuns', projectId, 'detail', runId] as const,
  scorecard: (projectId: string, runId: string) => ['evalRuns', projectId, 'scorecard', runId] as const,
};

function enabled(projectId: string | undefined): boolean {
  return projectId !== undefined && projectId !== '';
}

export function useEvalDatasets(
  projectId: string | undefined,
  agentId: number | undefined,
): UseQueryResult<EvalDataset[]> {
  return useQuery<EvalDataset[]>({
    queryKey: evalRunQueryKeys.datasetList(projectId ?? '', agentId),
    queryFn: () => fetchEvalDatasets(projectId ?? '', agentId),
    enabled: enabled(projectId),
  });
}

export function useEvalDataset(
  projectId: string | undefined,
  datasetId: string | undefined,
): UseQueryResult<EvalDatasetDetail> {
  return useQuery<EvalDatasetDetail>({
    queryKey: evalRunQueryKeys.dataset(projectId ?? '', datasetId ?? ''),
    queryFn: () => fetchEvalDataset(projectId ?? '', datasetId ?? ''),
    enabled: enabled(projectId) && datasetId !== undefined && datasetId !== '',
  });
}

export function useEvalRuns(
  projectId: string | undefined,
  agentId: number | undefined,
): UseQueryResult<EvalRun[]> {
  return useQuery<EvalRun[]>({
    queryKey: evalRunQueryKeys.runList(projectId ?? '', agentId),
    queryFn: () => fetchEvalRuns(projectId ?? '', agentId),
    enabled: enabled(projectId),
    // The LISTING follows an active run too. Without this the run a person
    // just started sits at `created` until they navigate away and back, which
    // reads as a run that never started.
    refetchInterval: (query) => {
      const runs = query.state.data;
      if (runs === undefined) return false;
      return runs.some((run) => !isRunTerminal(run.status)) ? RUN_POLL_INTERVAL_MS : false;
    },
  });
}

export function useEvalRun(
  projectId: string | undefined,
  runId: string | undefined,
): UseQueryResult<EvalRun> {
  return useQuery<EvalRun>({
    queryKey: evalRunQueryKeys.run(projectId ?? '', runId ?? ''),
    queryFn: () => fetchEvalRun(projectId ?? '', runId ?? ''),
    enabled: enabled(projectId) && runId !== undefined && runId !== '',
    // The poll STOPS ITSELF at a terminal status. An interval that kept
    // running would poll a finished run for as long as the tab stayed open.
    refetchInterval: (query) => (isRunTerminal(query.state.data?.status) ? false : RUN_POLL_INTERVAL_MS),
  });
}

export function useEvalScorecard(
  projectId: string | undefined,
  runId: string | undefined,
): UseQueryResult<EvalScorecard> {
  return useQuery<EvalScorecard>({
    queryKey: evalRunQueryKeys.scorecard(projectId ?? '', runId ?? ''),
    queryFn: () => fetchScorecard(projectId ?? '', runId ?? ''),
    enabled: enabled(projectId) && runId !== undefined && runId !== '',
  });
}

interface AddCaseArgs {
  readonly datasetId: string;
  readonly input: EvalDatasetCaseWriteRequest;
}

interface RemoveCaseArgs {
  readonly datasetId: string;
  readonly caseId: string;
}

export function useEvalDatasetMutations(projectId: string | undefined) {
  const queryClient = useQueryClient();
  const invalidateDatasets = async (): Promise<void> => {
    if (!enabled(projectId)) return;
    await queryClient.invalidateQueries({ queryKey: evalRunQueryKeys.datasets(projectId ?? '') });
  };

  return {
    create: useMutation({
      mutationFn: (input: EvalDatasetWriteRequest) => createDataset(projectId ?? '', input),
      onSuccess: invalidateDatasets,
    }),
    remove: useMutation({
      mutationFn: (datasetId: string) => removeDataset(projectId ?? '', datasetId),
      onSuccess: invalidateDatasets,
    }),
    addCase: useMutation({
      mutationFn: (args: AddCaseArgs) => addCase(projectId ?? '', args.datasetId, args.input),
      // The DETAIL and the LIST both change: the detail gains a row and the
      // list's `case_count` badge moves. Invalidating the whole dataset
      // namespace covers both, which is why the keys are nested under one root.
      onSuccess: invalidateDatasets,
    }),
    removeCase: useMutation({
      mutationFn: (args: RemoveCaseArgs) => removeCase(projectId ?? '', args.datasetId, args.caseId),
      onSuccess: invalidateDatasets,
    }),
  };
}

export function useEvalRunMutations(projectId: string | undefined) {
  const queryClient = useQueryClient();
  const invalidateRuns = async (): Promise<void> => {
    if (!enabled(projectId)) return;
    await queryClient.invalidateQueries({ queryKey: evalRunQueryKeys.runs(projectId ?? '') });
  };

  return {
    start: useMutation({
      mutationFn: (input: EvalRunStartRequest) => startRun(projectId ?? '', input),
      onSuccess: invalidateRuns,
    }),
    cancel: useMutation({
      mutationFn: (runId: string) => cancelRun(projectId ?? '', runId),
      onSuccess: invalidateRuns,
    }),
  };
}
