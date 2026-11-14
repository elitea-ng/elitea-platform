/**
 * A run's insights (issue 2011, step A): consumption, efficiency and the
 * comparison with the previous run on the same dataset.
 *
 * A run of one model call per case (`predict_blocking`) has no trajectory or
 * tool metric to show. A run that executed its cases as agent runs
 * (`runtime_plane`, step B) adds the Trajectory card and the tool metrics of the
 * Efficiency card, both read from the per-step run evidence stored with each
 * case.
 */
import { useMemo, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { EliteaApiError } from '@/shared/api/generated/mutator';
import type { EvalScorecard } from '@/shared/api/generated/model';

import {
  caseTurns,
  compareRuns,
  summarizeEfficiency,
} from '../lib/runInsights';
import { summarizeTrajectory } from '../lib/trajectory';
import { isRunTerminal } from '../model/useEvalRuns';
import { usePreviousRun, useRunAnalytics } from '../model/useRunInsights';
import { ComparisonCard, ConsumptionCard, EfficiencyStrip } from './RunInsightCards';
import { TrajectoryCard } from './TrajectoryCard';

const rootSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem' };

function gapOf(error: unknown): 'forbidden' | 'unavailable' | undefined {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return undefined;
  if (error.failure.status === 403) return 'forbidden';
  return error.failure.status === 501 || error.failure.status === 404 ? 'unavailable' : undefined;
}

export interface RunInsightsProps {
  readonly projectId: string | undefined;
  readonly scorecard: EvalScorecard;
  /** Dimension names by id, from the run's frozen snapshot. */
  readonly dimensionNames: ReadonlyMap<string, string>;
}

export function RunInsights({ projectId, scorecard, dimensionNames }: RunInsightsProps): ReactNode {
  const run = scorecard.run;
  // A run still in flight has partial figures: they are shown once it stops.
  const settled = isRunTerminal(run.status);
  const analyticsQuery = useRunAnalytics(projectId, run.id, settled);
  const { previous, scorecard: previousScorecard, loading } = usePreviousRun(projectId, run, settled);
  const previousAnalytics = useRunAnalytics(projectId, previous?.id, settled && previous !== undefined);

  const efficiency = useMemo(
    () => summarizeEfficiency(caseTurns(scorecard.results), analyticsQuery.data),
    [scorecard.results, analyticsQuery.data],
  );
  const isAgentRun = run.execution_mode === 'runtime_plane';
  const trajectory = useMemo(
    () => (isAgentRun ? summarizeTrajectory(scorecard.results) : undefined),
    [isAgentRun, scorecard.results],
  );
  const comparison = useMemo(() => {
    if (previous === undefined || previousScorecard.data === undefined) return undefined;
    const previousEfficiency = summarizeEfficiency(caseTurns(previousScorecard.data.results), previousAnalytics.data);
    return compareRuns(
      { run, results: scorecard.results, efficiency },
      { run: previous, results: previousScorecard.data.results, efficiency: previousEfficiency },
    );
  }, [run, scorecard.results, efficiency, previous, previousScorecard.data, previousAnalytics.data]);

  if (!settled) return null;
  return (
    <Box sx={rootSx} data-testid="run-insights">
      <ConsumptionCard analytics={analyticsQuery.data} gap={gapOf(analyticsQuery.error)} />
      <EfficiencyStrip efficiency={efficiency} trajectory={trajectory} />
      {isAgentRun && <TrajectoryCard summary={trajectory} />}
      <ComparisonCard comparison={comparison} noPrevious={previous === undefined && !loading} dimensionNames={dimensionNames} />
    </Box>
  );
}
