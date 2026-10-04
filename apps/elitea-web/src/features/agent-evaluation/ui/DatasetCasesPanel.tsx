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
 *
 * INCLUDE / EXCLUDE (legacy issue 6700). Each case has a checkbox. A cleared
 * checkbox EXCLUDES the case: it stays in the dataset, a run leaves it out,
 * and its text takes the disabled colour. The "N active" chip counts the
 * cases a run will execute. There is no confirmation: the toggle is
 * reversible and deletes nothing.
 */
import { useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { EvalDatasetCase } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

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
// The checkbox has no padding of its own, so its box lines up with the first
// line of the case input and not with the space above it.
const caseCheckboxSx: SxProps<Theme> = { padding: 0, marginTop: '0.125rem' };
const headerSx: SxProps<Theme> = { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: '0.5rem' };
const titleSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem' };
const caseTextSx: SxProps<Theme> = { flexGrow: 1, minWidth: 0 };
const excludedTextSx: SxProps<Theme> = (theme: Theme) => ({
  flexGrow: 1,
  minWidth: 0,
  color: theme.vars.palette.text.button.disabled,
});
const formSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };

interface DatasetCaseRowProps {
  readonly datasetCase: EvalDatasetCase;
  readonly canEdit: boolean;
  readonly isToggling: boolean;
  /** `true` excludes the case, `false` includes it again. */
  readonly onToggle: (excluded: boolean) => void;
  readonly onRemove: () => void;
}

function DatasetCaseRow(props: DatasetCaseRowProps): ReactNode {
  const { datasetCase, canEdit, isToggling, onToggle, onRemove } = props;
  const isExcluded = datasetCase.excluded;
  return (
    <Box sx={caseRowSx} data-testid={`dataset-case-${datasetCase.id}`}>
      <BaseCheckbox
        sx={caseCheckboxSx}
        checked={!isExcluded}
        disabled={!canEdit || isToggling}
        data-testid={`dataset-case-include-${datasetCase.id}`}
        aria-label={t('features.agentEvaluation.cases.include', 'Include this case in runs')}
        onChange={(event) => onToggle(!event.target.checked)}
      />
      <Box sx={isExcluded ? excludedTextSx : caseTextSx} data-excluded={isExcluded ? 'true' : undefined}>
        <Typography variant="bodyMedium" color={isExcluded ? 'inherit' : undefined}>
          {datasetCase.input}
        </Typography>
        {/*
          An absent expected answer is SAID so, rather than left blank. A
          blank cell reads as "the author wrote nothing here", which is the
          same thing an empty expected answer would look like — and the two
          mean different things to the judge.
        */}
        <Typography variant="bodySmall" color={isExcluded ? 'inherit' : 'text.secondary'}>
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
          onClick={onRemove}
        >
          ×
        </IconButton>
      )}
    </Box>
  );
}

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
  const activeCount = cases.filter((datasetCase) => !datasetCase.excluded).length;
  const addError = datasetErrorMessage(mutations.addCase.error);
  // The toggle and the remove button fail SILENTLY without these: there is
  // no optimistic update, so a refused PUT or DELETE (no dataset.update, a
  // case another tab deleted, a network failure) just leaves the row as it
  // was, and the author cannot tell why the click did nothing.
  const toggleError = datasetErrorMessage(mutations.setCaseExcluded.error);
  const removeError = datasetErrorMessage(mutations.removeCase.error);

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
      <Box sx={headerSx}>
        <Box sx={titleSx}>
          <Typography variant="labelMedium">
            {t('features.agentEvaluation.cases.title', 'Cases')}
          </Typography>
          {cases.length > 0 && (
            <Chip
              size="small"
              data-testid="dataset-cases-active-count"
              label={t('features.agentEvaluation.cases.activeCount', '{{count}} active', {
                count: activeCount,
              })}
            />
          )}
        </Box>
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
        <DatasetCaseRow
          key={datasetCase.id}
          datasetCase={datasetCase}
          canEdit={canEdit}
          isToggling={mutations.setCaseExcluded.isPending}
          onToggle={(excluded) => mutations.setCaseExcluded.mutate({ datasetId, caseId: datasetCase.id, excluded })}
          onRemove={() => mutations.removeCase.mutate({ datasetId, caseId: datasetCase.id })}
        />
      ))}

      {toggleError !== undefined && (
        <Typography role="alert" variant="bodyMedium" component="p" color="error" data-testid="dataset-case-toggle-error">
          {toggleError}
        </Typography>
      )}
      {removeError !== undefined && (
        <Typography role="alert" variant="bodyMedium" component="p" color="error" data-testid="dataset-case-remove-error">
          {removeError}
        </Typography>
      )}

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
            <Typography role="alert" variant="bodyMedium" component="p" color="error" data-testid="dataset-case-error">
              {addError}
            </Typography>
          )}
        </Box>
      )}
    </Box>
  );
}
