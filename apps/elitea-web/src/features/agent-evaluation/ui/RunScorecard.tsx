/**
 * One run's scorecard. READ-ONLY.
 *
 * There is no human-score input in this release, and the response OMITS
 * `human_scores` rather than answering `[]`. That omission is rendered, not
 * swallowed: `unavailable` names each missing capability with its reason, and a
 * reader who cannot see human scores is told the deployment cannot have any
 * rather than being left to conclude nobody has entered one.
 *
 * A CASE THAT COULD NOT BE SCORED shows its status and its verdict, never a
 * zero. Zero is a real and very bad score; printing it for an unscorable case
 * would report a bad prompt as a bad agent.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { EvalRun, EvalRunResult, EvalScorecard } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { isRunTerminal, useEvalRun, useEvalScorecard } from '../model/useEvalRuns';

const rowSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'flex-start',
  gap: '0.75rem',
  padding: '0.25rem 0',
};

/**
 * The reason text on a result, if it has one.
 *
 * The verdict is free-form jsonb, so `verdict.reason` and `verdict.error` are
 * both strings the server may write, and both are what a person reads: the
 * first explains a score, the second explains why there is none.
 */
function resultReason(result: EvalRunResult): string | undefined {
  const verdict: Record<string, unknown> | undefined = result.verdict;
  if (verdict === undefined) return undefined;
  const reason = verdict['reason'] ?? verdict['error'];
  return typeof reason === 'string' && reason !== '' ? reason : undefined;
}

/** The score cell: the normalised number, or the reason there is none. */
function resultScoreText(result: EvalRunResult): string {
  if (result.status !== 'ok' || result.normalized_score === null || result.normalized_score === undefined) {
    return t('features.agentEvaluation.scorecard.unscored', 'Not scored ({{status}})', {
      status: result.status,
    });
  }
  return t('features.agentEvaluation.scorecard.score', '{{score}} / 100', {
    score: result.normalized_score,
  });
}

interface ScorecardBodyProps {
  readonly scorecard: EvalScorecard;
  /** The LIVE run, from the progress poll — not the snapshot inside `scorecard`. */
  readonly liveRun: EvalRun | undefined;
}

/**
 * Everything inside the dialog. Split out of `RunScorecard` to keep that
 * function inside the §3.5 cyclomatic-complexity budget of 12, and because it
 * has no queries of its own: it is a pure function of the two values above.
 */
function ScorecardBody({ scorecard, liveRun }: ScorecardBodyProps): ReactNode {
  return (
    <>
      <Typography variant="bodySmall" color="text.secondary" data-testid="scorecard-progress">
        {t('features.agentEvaluation.runs.progress', '{{done}} of {{total}} case(s)', {
          done: liveRun?.progress.done ?? 0,
          total: liveRun?.progress.total ?? 0,
        })}
        {liveRun !== undefined && !isRunTerminal(liveRun.status)
          ? t('features.agentEvaluation.scorecard.stillRunning', ' — still running')
          : ''}
      </Typography>

      <Typography variant="labelMedium" data-testid="scorecard-headline">
        {scorecard.headline_score === null || scorecard.headline_score === undefined
          ? t('features.agentEvaluation.scorecard.noHeadline', 'This run produced no score.')
          : t('features.agentEvaluation.scorecard.headline', 'Overall {{score}} / 100', {
              score: scorecard.headline_score,
            })}
      </Typography>

      {/*
        WHAT ACTUALLY RAN. `predict_blocking` is one blocking LLM turn per case
        — no tools, no toolkits, no memory — so a reader must not take these
        numbers for a measurement of the whole agent.
      */}
      <Typography variant="bodySmall" color="text.secondary" data-testid="scorecard-execution-mode">
        {scorecard.run.execution_mode === 'predict_blocking'
          ? t(
              'features.agentEvaluation.scorecard.predictBlocking',
              'Scored by running the agent’s instructions as a single model turn. Tools and toolkits were not used.',
            )
          : t('features.agentEvaluation.scorecard.runtimePlane', 'Scored by running the agent.')}
      </Typography>

      {scorecard.results.length === 0 && (
        <Typography component="output" variant="bodyMedium">
          {t('features.agentEvaluation.scorecard.empty', 'This run has no results yet.')}
        </Typography>
      )}

      {scorecard.results.map((result) => (
        <Box key={result.id} sx={rowSx} data-testid={`scorecard-result-${result.id}`}>
          <Typography variant="bodySmall">
            {t('features.agentEvaluation.scorecard.caseLabel', 'Case {{id}}', {
              id: result.dataset_case_id,
            })}
          </Typography>
          <Typography variant="bodyMedium">{resultScoreText(result)}</Typography>
          {resultReason(result) !== undefined && (
            <Typography variant="bodySmall" color="text.secondary">
              {resultReason(result)}
            </Typography>
          )}
        </Box>
      ))}

      {/*
        The named gaps. Rendering them is what stops "no human scores shown"
        from reading as "nobody has entered one".
      */}
      {scorecard.unavailable.length > 0 && (
        <Box data-testid="scorecard-unavailable">
          <Typography variant="bodySmall" color="text.secondary">
            {t('features.agentEvaluation.scorecard.unavailableTitle', 'Not served by this release:')}
          </Typography>
          {scorecard.unavailable.map((gap) => (
            <Typography key={gap.key} variant="bodySmall" color="text.secondary">
              {gap.reason}
            </Typography>
          ))}
        </Box>
      )}
    </>
  );
}

export interface RunScorecardProps {
  readonly projectId: string | undefined;
  /** `undefined` keeps the dialog closed. */
  readonly runId: string | undefined;
  readonly onClose: () => void;
}

export function RunScorecard(props: RunScorecardProps): ReactNode {
  const { projectId, runId, onClose } = props;
  /*
   * TWO READS, and both are needed.
   *
   * `useEvalRun` is the PROGRESS FEED — it polls `GET eval_run` while the run
   * is active and stops itself at a terminal status. `useEvalScorecard` is the
   * results table, and the `run` embedded in its response is a SNAPSHOT of the
   * moment that page was fetched. Rendering the header from the snapshot would
   * leave a scorecard opened on a running run showing "1 of 10" for ever, which
   * reads as a run that stopped.
   */
  const runQuery = useEvalRun(projectId, runId);
  const scorecardQuery = useEvalScorecard(projectId, runId);
  const scorecard = scorecardQuery.data;
  const liveRun = runQuery.data ?? scorecard?.run;

  return (
    <BaseModal
      open={runId !== undefined}
      variant="complex"
      data-testid="run-scorecard"
      title={t('features.agentEvaluation.scorecard.title', 'Scorecard')}
      onClose={onClose}
      actions={{ confirmText: t('features.agentEvaluation.scorecard.close', 'Close') }}
      onConfirm={onClose}
      content={
        <Box>
          {scorecardQuery.isPending && runId !== undefined && (
            <CircularProgress
              aria-label={t('features.agentEvaluation.scorecard.loading', 'Loading the scorecard')}
            />
          )}
          {scorecardQuery.isError && (
            <Typography role="alert" variant="bodyMedium">
              {t('features.agentEvaluation.scorecard.loadFailed', 'Failed to load this run’s scorecard.')}
            </Typography>
          )}

          {scorecard !== undefined && <ScorecardBody scorecard={scorecard} liveRun={liveRun} />}
        </Box>
      }
    />
  );
}
