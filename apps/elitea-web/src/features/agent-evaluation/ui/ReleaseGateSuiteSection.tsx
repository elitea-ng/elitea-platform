import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ReleaseGateSuite } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { releaseGateErrorMessage, usePutReleaseGateSuite, useReleaseGateSuites } from '../api/releaseGateApi';
import { draftFromSuite, draftProblem, requestFromDraft, type ReleaseGateDraft } from '../lib/releaseGateForm';
import { useEvalDimensions } from '../model/useEvalDimensions';
import { ReleaseGateCriteriaEditor } from './ReleaseGateCriteriaEditor';

/**
 * Release gate settings of the selected dataset (ADR-0040 decision 4, #1497).
 *
 * A dataset marked as a release gate suite is run against every release
 * candidate when its change request is submitted; the gate passes when every
 * criterion holds. The regression suite (one per agent) also runs against the
 * current release, and a case that passed there and fails on the candidate is
 * a regression. The cadence re-runs the agent's suites against the current
 * release; a regression there marks the certification due.
 */
export interface ReleaseGateSuiteSectionProps {
  readonly projectId: string | undefined;
  readonly applicationId: number;
  /** The dataset id, a numeric string on the wire. */
  readonly datasetId: string;
  readonly canEdit: boolean;
}

export function ReleaseGateSuiteSection(props: ReleaseGateSuiteSectionProps): ReactNode {
  const suites = useReleaseGateSuites(props.projectId, props.applicationId);
  if (suites.isPending) return null;
  const suite = suites.data?.rows.find((row) => String(row.dataset_id) === props.datasetId);
  // Remounted when the stored suite changes, so the form starts from it.
  return (
    <ReleaseGateForm
      key={`${props.datasetId}:${suite?.updated_at ?? 'none'}`}
      {...props}
      suite={suite}
    />
  );
}

function ReleaseGateForm({
  projectId,
  applicationId,
  datasetId,
  canEdit,
  suite,
}: ReleaseGateSuiteSectionProps & { readonly suite: ReleaseGateSuite | undefined }): ReactNode {
  const [draft, setDraft] = useState<ReleaseGateDraft>(() => draftFromSuite(suite));
  const dimensions = useEvalDimensions(projectId, applicationId);
  const save = usePutReleaseGateSuite(projectId, applicationId);
  const problem = draftProblem(draft);
  const disabled = !canEdit || save.isPending;

  return (
    <Box
      sx={sectionSx}
      data-testid="release-gate-suite"
    >
      <Typography variant="labelMedium">
        {t('features.agentEvaluation.releaseGate.title', 'Release gate')}
      </Typography>
      <Box sx={rowSx}>
        <FormControlLabel
          control={
            <Switch
              checked={draft.releaseGate}
              disabled={disabled}
              onChange={(event) => setDraft({ ...draft, releaseGate: event.target.checked })}
              slotProps={{ input: { 'aria-label': t('features.agentEvaluation.releaseGate.mark', 'Release gate suite') } }}
              data-testid="release-gate-toggle"
            />
          }
          label={t('features.agentEvaluation.releaseGate.mark', 'Release gate suite')}
        />
        <FormControlLabel
          control={
            <Switch
              checked={draft.regression}
              disabled={disabled || !draft.releaseGate}
              onChange={(event) => setDraft({ ...draft, regression: event.target.checked })}
              data-testid="release-gate-regression"
            />
          }
          label={t('features.agentEvaluation.releaseGate.regression', 'Regression suite')}
        />
      </Box>
      {suite?.dataset_missing === true && (
        <Alert severity="warning">
          {t('features.agentEvaluation.releaseGate.datasetMissing', 'This dataset was deleted after it was marked; gates fail it.')}
        </Alert>
      )}
      {draft.releaseGate && (
        <>
          <ReleaseGateCriteriaEditor
            criteria={draft.criteria}
            dimensions={dimensions.data ?? []}
            disabled={disabled}
            onChange={(criteria) => setDraft({ ...draft, criteria })}
          />
          <TextField
            size="small"
            type="number"
            label={t('features.agentEvaluation.releaseGate.cadence', 'Re-evaluate every (days)')}
            helperText={t('features.agentEvaluation.releaseGate.cadenceHelp', 'Blank turns the scheduled re-evaluation off.')}
            value={draft.reevaluateEveryDays}
            disabled={disabled}
            onChange={(event) => setDraft({ ...draft, reevaluateEveryDays: event.target.value })}
            slotProps={{ htmlInput: { min: 1, max: 365, 'data-testid': 'release-gate-cadence' } }}
            sx={{ width: '14rem' }}
          />
        </>
      )}
      {problem !== undefined && <ProblemText problem={problem} />}
      {save.error !== null && <Alert severity="error">{releaseGateErrorMessage(save.error)}</Alert>}
      {canEdit && (
        <Box>
          <Button
            size="small"
            variant="contained"
            disabled={disabled || problem !== undefined}
            onClick={() => save.mutate({ datasetId: Number(datasetId), body: requestFromDraft(draft) })}
            data-testid="release-gate-save"
          >
            {t('features.agentEvaluation.releaseGate.save', 'Save gate settings')}
          </Button>
        </Box>
      )}
    </Box>
  );
}

function ProblemText({ problem }: { readonly problem: NonNullable<ReturnType<typeof draftProblem>> }): ReactNode {
  const text = {
    criteria: t('features.agentEvaluation.releaseGate.needCriteria', 'Add one to five criteria.'),
    duplicate: t('features.agentEvaluation.releaseGate.duplicate', 'Use each dimension once.'),
    value: t('features.agentEvaluation.releaseGate.badValue', 'Every criterion needs a number from 0 to 100.'),
    cadence: t('features.agentEvaluation.releaseGate.badCadence', 'The cadence is a whole number of days from 1 to 365.'),
  }[problem];
  return (
    <Typography
      variant="bodySmall"
      color="text.secondary"
      data-testid="release-gate-problem"
    >
      {text}
    </Typography>
  );
}

const sectionSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  padding: '0.75rem',
  border: `1px solid ${theme.vars.palette.divider}`,
  borderRadius: 'var(--el-shape-radiusSm, 4px)',
});
const rowSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '1rem' };
