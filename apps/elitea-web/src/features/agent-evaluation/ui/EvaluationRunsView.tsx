/**
 * The RUNS sub-view of the agent editor's Evaluation tab: start a run, follow
 * it, open its scorecard.
 *
 * FOLLOWED BY POLLING, NOT BY A SOCKET. The reference subscribes to an
 * `eval_run_progress` namespace; there is no socket server in this platform
 * (#126 deleted `internal/api/socketio`, #615 recorded the decision), and the
 * reference's own hook already documents a polling fallback and refuses to
 * trust `socketConnected` without a join ack. `GET eval_run` carries
 * `progress{done,total}` and the status, so the poll is a port and not a
 * downgrade — see `useEvalRuns`, where it stops itself at a terminal status.
 */
import { useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { useEvalDimensions } from '../model/useEvalDimensions';
import { isRunTerminal, useEvalDatasets, useEvalRunMutations, useEvalRuns } from '../model/useEvalRuns';
import { useEvaluationPermissions } from '../model/useEvaluationPermissions';
import { EvaluationStatus } from './EvaluationStatus';
import { RunScorecard } from './RunScorecard';
import { RunStartForm } from './RunStartForm';

const rootSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const runRowSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '0.75rem',
  padding: '0.5rem 0.75rem',
};

export interface EvaluationRunsViewProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  /** The version a new run scores. Without it there is nothing to run. */
  readonly applicationVersionId: number | undefined;
}

export function EvaluationRunsView(props: EvaluationRunsViewProps): ReactNode {
  const { projectId, applicationId, applicationVersionId } = props;
  const permissions = useEvaluationPermissions(projectId);
  const [openScorecardId, setOpenScorecardId] = useState<string | undefined>(undefined);

  const runsQuery = useEvalRuns(permissions.canReadRuns ? projectId : undefined, applicationId);
  const datasetsQuery = useEvalDatasets(permissions.canReadDatasets ? projectId : undefined, applicationId);
  const dimensionsQuery = useEvalDimensions(permissions.canRead ? projectId : undefined, applicationId);
  const mutations = useEvalRunMutations(projectId);

  // Only `ai` dimensions can be scored by this release. Offering a `code` one
  // would produce a 501 the person could not have predicted from the form.
  const aiDimensions = useMemo(
    () => (dimensionsQuery.data ?? []).filter((dimension) => dimension.allowed_engines.includes('ai')),
    [dimensionsQuery.data],
  );

  if (!permissions.canReadRuns) {
    return (
      <Typography component="output" variant="bodyMedium">
        {t(
          'features.agentEvaluation.runs.noReadPermission',
          'You do not have permission to view this project’s evaluation runs.',
        )}
      </Typography>
    );
  }

  const runs = runsQuery.data ?? [];

  return (
    <Box sx={rootSx} data-testid="evaluation-runs-view">
      <Typography variant="labelMedium">{t('features.agentEvaluation.runs.title', 'Runs')}</Typography>

      {permissions.canStartRuns && (
        <RunStartForm
          datasets={datasetsQuery.data ?? []}
          dimensions={aiDimensions}
          applicationId={applicationId}
          applicationVersionId={applicationVersionId}
          isStarting={mutations.start.isPending}
          error={mutations.start.error}
          onStart={(input) => mutations.start.mutate(input)}
        />
      )}

      {runsQuery.isPending ? (
        <CircularProgress aria-label={t('features.agentEvaluation.runs.loading', 'Loading runs')} />
      ) : (
        <EvaluationStatus
          isError={runsQuery.isError}
          isEmpty={runs.length === 0}
          errorText={t('features.agentEvaluation.runs.loadFailed', 'Failed to load the evaluation runs.')}
          emptyText={t('features.agentEvaluation.runs.empty', 'No runs yet.')}
        />
      )}

      {runs.map((run) => (
        <Box key={run.id} sx={runRowSx} data-testid={`evaluation-run-row-${run.id}`}>
          <Typography variant="bodyMedium">{run.status}</Typography>
          {/*
            The PROGRESS is shown for every run, terminal or not. A run that
            finished having scored two of ten cases is a different fact from a
            run that scored all ten, and the headline alone cannot say which.
          */}
          <Typography variant="bodySmall" color="text.secondary">
            {t('features.agentEvaluation.runs.progress', '{{done}} of {{total}} case(s)', {
              done: run.progress.done,
              total: run.progress.total,
            })}
          </Typography>
          {/*
            A null headline is SAID, not printed as 0. Zero is a real and very
            bad score, and a run that scored nothing has not earned it.
          */}
          <Typography variant="bodySmall">
            {run.headline_score === null || run.headline_score === undefined
              ? t('features.agentEvaluation.runs.noScore', 'No score')
              : t('features.agentEvaluation.runs.score', 'Score {{score}}', { score: run.headline_score })}
          </Typography>
          {run.error !== undefined && run.error !== '' && (
            <Typography role="alert" variant="bodySmall" color="error">
              {run.error}
            </Typography>
          )}
          <BaseBtn
            variant="text"
            data-testid={`evaluation-run-open-${run.id}`}
            onClick={() => setOpenScorecardId(run.id)}
          >
            {t('features.agentEvaluation.runs.openScorecard', 'Scorecard')}
          </BaseBtn>
          {permissions.canStartRuns && !isRunTerminal(run.status) && (
            <BaseBtn
              variant="text"
              data-testid={`evaluation-run-cancel-${run.id}`}
              onClick={() => mutations.cancel.mutate(run.id)}
            >
              {t('features.agentEvaluation.runs.cancel', 'Stop')}
            </BaseBtn>
          )}
        </Box>
      ))}

      <RunScorecard
        projectId={projectId}
        runId={openScorecardId}
        onClose={() => setOpenScorecardId(undefined)}
      />
    </Box>
  );
}
