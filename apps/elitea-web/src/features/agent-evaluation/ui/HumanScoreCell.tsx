/**
 * The human score of one result: what the reviewer already gave, and the
 * control to give one.
 *
 * A result bound to the human engine waits as `pending_human`. Saving a score
 * turns it into a score on the scorecard and moves the run's headline; the
 * scores are append-only, so a second save is a re-score that keeps the first
 * in the history.
 *
 * The score is checked against the scale FROZEN IN THE RUN before the request,
 * and the server checks it again: a 7 on a 1 to 5 scale is refused, not clamped.
 */
import { useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { EvalHumanScore, EvalRun, EvalRunResult, EvalRunSnapshotDimension } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { humanScoreErrorMessage } from '../lib/evaluationError';
import { useEvalHumanScores, useRecordHumanScore } from '../model/useEvalSuites';
import { useEvaluationPermissions } from '../model/useEvaluationPermissions';

/** Why `raw` is not a score on this dimension's scale, or `undefined`. */
function scoreProblem(raw: string, dimension: EvalRunSnapshotDimension | undefined): string | undefined {
  const text = raw.trim();
  if (text === '' || dimension === undefined) return undefined;
  const value = Number(text);
  if (!Number.isFinite(value)) return t('features.agentEvaluation.humanScore.notNumber', 'Enter a number.');
  if (dimension.scale_type === 'binary') {
    return value === 0 || value === 1
      ? undefined
      : t('features.agentEvaluation.humanScore.binary', 'Score 0 or 1.');
  }
  const min = dimension.scale_min ?? 0;
  const max = dimension.scale_max ?? 0;
  return value >= min && value <= max
    ? undefined
    : t('features.agentEvaluation.humanScore.range', 'Score between {{min}} and {{max}}.', { min, max });
}

function LatestHumanScore(props: {
  readonly latest: EvalHumanScore | undefined;
  /** The result already carries a human score (status ok, scored_by human). */
  readonly scored: boolean;
}): ReactNode {
  const { latest, scored } = props;
  if (latest === undefined && scored) {
    // The caller may not read the score history: the result still says it was scored.
    return (
      <Typography variant="bodySmall" color="text.secondary" data-testid="human-score-latest">
        {t('features.agentEvaluation.humanScore.scoredNoHistory', 'Scored by a reviewer.')}
      </Typography>
    );
  }
  if (latest === undefined) {
    return (
      <Typography variant="bodySmall" color="text.secondary">
        {t('features.agentEvaluation.humanScore.waiting', 'Waiting for a human score.')}
      </Typography>
    );
  }
  return (
    <Typography variant="bodySmall" color="text.secondary" data-testid="human-score-latest">
      {t('features.agentEvaluation.humanScore.latest', 'Scored {{score}} by reviewer {{reviewer}}{{note}}', {
        score: latest.native_score,
        reviewer: latest.reviewer_id ?? t('features.agentEvaluation.humanScore.unknownReviewer', 'unknown'),
        note: latest.note === undefined || latest.note === '' ? '' : `: ${latest.note}`,
      })}
    </Typography>
  );
}

interface HumanScoreFormProps {
  readonly projectId: string | undefined;
  readonly runId: string;
  readonly caseId: string;
  readonly dimensionId: string;
  readonly dimension: EvalRunSnapshotDimension | undefined;
  readonly isRescore: boolean;
}

function HumanScoreForm(props: HumanScoreFormProps): ReactNode {
  const { projectId, runId, caseId, dimensionId, dimension, isRescore } = props;
  const [score, setScore] = useState('');
  const [note, setNote] = useState('');
  const mutation = useRecordHumanScore(projectId, runId);
  const problem = scoreProblem(score, dimension);
  const serverError = humanScoreErrorMessage(mutation.error);
  const key = `${caseId}-${dimensionId}`;

  const save = (): void => {
    if (score.trim() === '' || problem !== undefined) return;
    mutation.mutate(
      { dataset_case_id: caseId, dimension_id: dimensionId, score: Number(score), note: note.trim() },
      {
        onSuccess: () => {
          setScore('');
          setNote('');
        },
      },
    );
  };

  return (
    <>
      <Box sx={{ display: 'flex', gap: '0.5rem', alignItems: 'flex-start' }}>
        <TextField
          size="small"
          label={t('features.agentEvaluation.humanScore.scoreLabel', 'Score')}
          value={score}
          error={problem !== undefined}
          helperText={problem}
          slotProps={{ htmlInput: { 'data-testid': `human-score-input-${key}`, inputMode: 'decimal' } }}
          onChange={(event) => setScore(event.target.value)}
        />
        <TextField
          size="small"
          label={t('features.agentEvaluation.humanScore.noteLabel', 'Note')}
          value={note}
          slotProps={{ htmlInput: { 'data-testid': `human-score-note-${key}`, maxLength: 2000 } }}
          onChange={(event) => setNote(event.target.value)}
        />
        <BaseBtn
          variant="contained"
          data-testid={`human-score-save-${key}`}
          disabled={score.trim() === '' || problem !== undefined || mutation.isPending}
          onClick={save}
        >
          {isRescore
            ? t('features.agentEvaluation.humanScore.rescore', 'Re-score')
            : t('features.agentEvaluation.humanScore.save', 'Save score')}
        </BaseBtn>
      </Box>
      {serverError !== undefined && (
        <Typography role="alert" variant="bodySmall" color="error">
          {serverError}
        </Typography>
      )}
    </>
  );
}

interface HumanScoreCellProps {
  readonly projectId: string | undefined;
  /** The run the result belongs to; its snapshot carries the scale the score is checked against. */
  readonly run: EvalRun;
  readonly result: EvalRunResult;
}

export function HumanScoreCell(props: HumanScoreCellProps): ReactNode {
  const { projectId, run, result } = props;
  const caseId = result.dataset_case_id;
  const dimensionId = result.dimension_id;
  const permissions = useEvaluationPermissions(projectId);
  const history = (
    useEvalHumanScores(permissions.canReadHumanScores ? projectId : undefined, run.id).data ?? []
  ).filter((entry) => entry.dataset_case_id === caseId && entry.dimension_id === dimensionId);
  const latest = history.at(-1);
  const isScored = latest !== undefined || (result.status === 'ok' && result.verdict?.['scored_by'] === 'human');

  return (
    <Box
      sx={{ display: 'flex', flexDirection: 'column', gap: '0.25rem' }}
      data-testid={`human-score-${caseId}-${dimensionId}`}
    >
      <LatestHumanScore latest={latest} scored={isScored} />
      {permissions.canScoreHuman && (
        <HumanScoreForm
          projectId={projectId}
          runId={run.id}
          caseId={caseId}
          dimensionId={dimensionId}
          dimension={run.snapshot?.dimensions?.[dimensionId]}
          isRescore={isScored}
        />
      )}
    </Box>
  );
}
