/**
 * One dataset's cases: what the agent will be asked, and what the author
 * expected.
 *
 * A dataset with no cases cannot be run — the start route refuses it with a
 * 400 — so this panel is not an optional extra on top of "create a dataset".
 * It is the half that makes the dataset usable.
 *
 * `expected_output` is sent as `null` when the author left it blank, and NOT as
 * `""`. The two are different instructions to a judge: an expected answer of
 * empty string tells it to mark every non-empty answer wrong.
 */
import { useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { datasetErrorMessage } from '../lib/evaluationError';
import { useEvalDataset, useEvalDatasetMutations } from '../model/useEvalRuns';
import { EvaluationStatus } from './EvaluationStatus';

const panelSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  padding: '0.75rem',
  border: 1,
  borderColor: 'divider',
  borderRadius: 'var(--el-shape-radiusSm, 4px)',
};
const caseRowSx: SxProps<Theme> = { display: 'flex', alignItems: 'flex-start', gap: '0.5rem' };
const formSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };

export interface DatasetCasesPanelProps {
  readonly projectId: string | undefined;
  readonly datasetId: string;
  readonly canEdit: boolean;
  readonly canDelete: boolean;
  readonly onDeleteDataset: () => void;
}

export function DatasetCasesPanel(props: DatasetCasesPanelProps): ReactNode {
  const { projectId, datasetId, canEdit, canDelete, onDeleteDataset } = props;
  const detailQuery = useEvalDataset(projectId, datasetId);
  const mutations = useEvalDatasetMutations(projectId);

  const [input, setInput] = useState('');
  const [expected, setExpected] = useState('');

  const detail = detailQuery.data;
  const cases = detail?.cases ?? [];
  const addError = datasetErrorMessage(mutations.addCase.error);

  const handleAdd = (): void => {
    const trimmed = input.trim();
    if (trimmed === '') return;
    mutations.addCase.mutate(
      {
        datasetId,
        input: {
          input: trimmed,
          variables: {},
          // `null`, not `''`. See the module note.
          expected_output: expected.trim() === '' ? null : expected.trim(),
        },
      },
      {
        onSuccess: () => {
          setInput('');
          setExpected('');
        },
      },
    );
  };

  return (
    <Box sx={panelSx} data-testid="dataset-cases-panel">
      <Box sx={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <Typography variant="labelMedium">
          {t('features.agentEvaluation.cases.title', 'Cases')}
        </Typography>
        {canDelete && (
          <BaseBtn
            variant="text"
            color="error"
            data-testid="dataset-delete"
            onClick={onDeleteDataset}
          >
            {t('features.agentEvaluation.datasets.delete', 'Delete dataset')}
          </BaseBtn>
        )}
      </Box>

      {detailQuery.isPending ? (
        <CircularProgress aria-label={t('features.agentEvaluation.cases.loading', 'Loading cases')} />
      ) : (
        <EvaluationStatus
          isError={detailQuery.isError}
          isEmpty={cases.length === 0}
          errorText={t('features.agentEvaluation.cases.loadFailed', 'Failed to load this dataset’s cases.')}
          emptyText={t(
            'features.agentEvaluation.cases.empty',
            'No cases yet. A run needs at least one question to ask.',
          )}
        />
      )}

      {cases.map((datasetCase) => (
        <Box key={datasetCase.id} sx={caseRowSx} data-testid={`dataset-case-${datasetCase.id}`}>
          <Box sx={{ flexGrow: 1 }}>
            <Typography variant="bodyMedium">{datasetCase.input}</Typography>
            {/*
              An absent expected answer is SAID so, rather than left blank. A
              blank cell reads as "the author wrote nothing here", which is the
              same thing an empty expected answer would look like — and the two
              mean different things to the judge.
            */}
            <Typography variant="bodySmall" color="text.secondary">
              {datasetCase.expected_output === null || datasetCase.expected_output === undefined
                ? t('features.agentEvaluation.cases.noExpected', 'No expected answer stated')
                : t('features.agentEvaluation.cases.expected', 'Expected: {{answer}}', {
                    answer: datasetCase.expected_output,
                  })}
            </Typography>
          </Box>
          {canEdit && (
            <IconButton
              size="small"
              data-testid={`dataset-case-remove-${datasetCase.id}`}
              aria-label={t('features.agentEvaluation.cases.remove', 'Remove case')}
              onClick={() => mutations.removeCase.mutate({ datasetId, caseId: datasetCase.id })}
            >
              ×
            </IconButton>
          )}
        </Box>
      ))}

      {canEdit && (
        <Box sx={formSx}>
          <TextField
            fullWidth
            multiline
            label={t('features.agentEvaluation.cases.inputLabel', 'What the agent is asked')}
            value={input}
            slotProps={{ htmlInput: { 'data-testid': 'dataset-case-input' } }}
            onChange={(event) => setInput(event.target.value)}
          />
          <TextField
            fullWidth
            multiline
            label={t('features.agentEvaluation.cases.expectedLabel', 'Expected answer (optional)')}
            value={expected}
            slotProps={{ htmlInput: { 'data-testid': 'dataset-case-expected' } }}
            onChange={(event) => setExpected(event.target.value)}
          />
          <BaseBtn
            variant="contained"
            data-testid="dataset-case-add"
            disabled={input.trim() === '' || mutations.addCase.isPending}
            onClick={handleAdd}
          >
            {t('features.agentEvaluation.cases.add', 'Add case')}
          </BaseBtn>
          {addError !== undefined && (
            <Typography role="alert" variant="body2" color="error" data-testid="dataset-case-error">
              {addError}
            </Typography>
          )}
        </Box>
      )}
    </Box>
  );
}
