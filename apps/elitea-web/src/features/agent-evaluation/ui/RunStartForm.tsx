/**
 * Start one evaluation run: pick a dataset, pick the dimensions, go.
 *
 * NO SUITE PICKER. There is no suite table in this release, so the dimensions
 * are chosen here and the server freezes their own defaults into the run's
 * snapshot. A picker for a concept the server does not have would be a control
 * whose selection nothing reads.
 *
 * THE START BUTTON IS DISABLED WITH A STATED REASON, never silently. A run
 * needs a version, a dataset with at least one case, and at least one `ai`
 * dimension; a button that does nothing when pressed is how each of those
 * reads as a broken feature.
 */
import { useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Checkbox from '@mui/material/Checkbox';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { EvalDataset, EvalRunStartRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { runErrorMessage } from '../lib/evaluationError';
import type { EvalDimension } from '../model/types';

const formSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  padding: '0.75rem',
  border: 1,
  borderColor: 'divider',
  borderRadius: 'var(--el-shape-radiusSm, 4px)',
};

/** The server's own bound (`MaxDimensionsPerRun`). Stated here so the form refuses before the request does. */
const MAX_DIMENSIONS_PER_RUN = 5;

export interface RunStartFormProps {
  readonly datasets: readonly EvalDataset[];
  /** Already filtered to the `ai` engine by the caller — this release scores nothing else. */
  readonly dimensions: readonly EvalDimension[];
  readonly applicationId: number | undefined;
  readonly applicationVersionId: number | undefined;
  readonly isStarting: boolean;
  readonly error: unknown;
  readonly onStart: (input: EvalRunStartRequest) => void;
}

/**
 * Why the form cannot be submitted, or `undefined`.
 *
 * The REASON is rendered, not only used to disable the button: "disabled" is
 * true for four different causes, and a person needs to know which one they are
 * looking at. `EvaluationRunsView.test.tsx` asserts on the rendered text for
 * that reason rather than on the disabled attribute.
 */
function runStartBlockedReason(props: {
  readonly datasetId: string;
  readonly dimensionIds: readonly string[];
  readonly datasets: readonly EvalDataset[];
  readonly applicationVersionId: number | undefined;
}): string | undefined {
  if (props.applicationVersionId === undefined) {
    return t(
      'features.agentEvaluation.runs.needVersion',
      'Save this agent before running an evaluation: a run scores one specific version.',
    );
  }
  if (props.datasetId === '') {
    return t('features.agentEvaluation.runs.needDataset', 'Choose a dataset.');
  }
  const dataset = props.datasets.find((entry) => entry.id === props.datasetId);
  if (dataset !== undefined && dataset.case_count === 0) {
    return t(
      'features.agentEvaluation.runs.needCases',
      'This dataset has no cases. Add at least one before starting a run.',
    );
  }
  if (props.dimensionIds.length === 0) {
    return t('features.agentEvaluation.runs.needDimension', 'Choose at least one dimension to score against.');
  }
  if (props.dimensionIds.length > MAX_DIMENSIONS_PER_RUN) {
    return t(
      'features.agentEvaluation.runs.tooManyDimensions',
      'A run may score against at most {{max}} dimensions.',
      { max: MAX_DIMENSIONS_PER_RUN },
    );
  }
  return undefined;
}

export function RunStartForm(props: RunStartFormProps): ReactNode {
  const { datasets, dimensions, applicationId, applicationVersionId, isStarting, error, onStart } = props;
  const [datasetId, setDatasetId] = useState('');
  const [dimensionIds, setDimensionIds] = useState<string[]>([]);

  const blockedReason = runStartBlockedReason({ datasetId, dimensionIds, datasets, applicationVersionId });
  // The SERVER's own refusal wins. A 501 names which engine a dimension needs
  // and why this release does not serve it; the transport message would say
  // only "501 from …", which nobody can act on. See lib/evaluationError.ts.
  const startError = runErrorMessage(error);

  const toggle = (id: string): void => {
    setDimensionIds((previous) =>
      previous.includes(id) ? previous.filter((entry) => entry !== id) : [...previous, id],
    );
  };

  return (
    <Box sx={formSx} data-testid="run-start-form">
      <TextField
        select
        fullWidth
        label={t('features.agentEvaluation.runs.datasetLabel', 'Dataset')}
        value={datasetId}
        slotProps={{ htmlInput: { 'data-testid': 'run-dataset-select' } }}
        onChange={(event) => setDatasetId(event.target.value)}
      >
        {datasets.map((dataset) => (
          <MenuItem key={dataset.id} value={dataset.id}>
            {dataset.name}
          </MenuItem>
        ))}
      </TextField>

      <Typography variant="bodySmall" color="text.secondary">
        {t('features.agentEvaluation.runs.dimensionsLabel', 'Score against')}
      </Typography>
      {dimensions.map((dimension) => (
        <FormControlLabel
          key={dimension.id}
          control={
            <Checkbox
              checked={dimensionIds.includes(dimension.id)}
              data-testid={`run-dimension-${dimension.id}`}
              onChange={() => toggle(dimension.id)}
            />
          }
          label={dimension.name}
        />
      ))}

      <BaseBtn
        variant="contained"
        data-testid="run-start"
        disabled={blockedReason !== undefined || isStarting}
        onClick={() => {
          if (blockedReason !== undefined || applicationVersionId === undefined) return;
          onStart({
            dataset_id: datasetId,
            application_id: applicationId ?? null,
            application_version_id: applicationVersionId,
            dimension_ids: dimensionIds,
            trigger_type: 'on_demand',
          });
        }}
      >
        {t('features.agentEvaluation.runs.start', 'Run evaluation')}
      </BaseBtn>

      {(startError ?? blockedReason) !== undefined && (
        <Typography role="alert" variant="body2" color="error" data-testid="run-start-error">
          {startError ?? blockedReason}
        </Typography>
      )}
    </Box>
  );
}
