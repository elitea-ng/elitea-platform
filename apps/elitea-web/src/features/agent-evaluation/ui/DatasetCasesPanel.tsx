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
 *
 * An excluded case keeps the two-tone hierarchy of the legacy disabled state
 * (gray10/gray20 dark, light10/light20 light): the input takes the primary
 * disabled tone and the expected line the dimmer one.
 *
 * Only the row being toggled is disabled while its PUT is in flight. The
 * mutation object reports the LATEST call alone, so the panel tracks the
 * in-flight case ids itself; reading `isPending` would lock every checkbox
 * and swallow a second click on another case.
 *
 * A hovered (or focused) row shows a "View details" button that opens the
 * case read-only: the fields are disabled and there is no Save or Cancel.
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
import { BaseModal } from '@/shared/ui/BaseModal';

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
const VIEW_DETAILS_CLASS = 'dataset-case-view-details';
// The button stays in the tab order (opacity, not visibility), so a keyboard
// user reaches it and `:focus-within` reveals it. A device without hover
// shows it all the time.
const caseRowSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'flex-start',
  gap: '0.5rem',
  [`& .${VIEW_DETAILS_CLASS}`]: { opacity: 0 },
  [`&:hover .${VIEW_DETAILS_CLASS}, &:focus-within .${VIEW_DETAILS_CLASS}`]: { opacity: 1 },
  '@media (hover: none)': { [`& .${VIEW_DETAILS_CLASS}`]: { opacity: 1 } },
};
// The checkbox has no padding of its own, so its box lines up with the first
// line of the case input and not with the space above it.
const caseCheckboxSx: SxProps<Theme> = { padding: 0, marginTop: '0.125rem' };
const headerSx: SxProps<Theme> = { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: '0.5rem' };
const titleSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem' };
const caseTextSx: SxProps<Theme> = { flexGrow: 1, minWidth: 0 };
// Legacy gray10 / light10: the primary line of a disabled item.
const excludedInputSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.primary });
// Legacy gray20 / light20: the secondary, dimmer line.
const excludedExpectedSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.button.disabled });
const detailsSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem', paddingTop: '0.5rem' };
const formSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };

interface CaseDetailsModalProps {
  readonly datasetCase: EvalDatasetCase | undefined;
  readonly onClose: () => void;
}

/** Legacy issue 6700 part 3: the case, read-only. No Save, no Cancel. */
function CaseDetailsModal({ datasetCase, onClose }: CaseDetailsModalProps): ReactNode {
  const variables = datasetCase?.variables ?? {};
  const hasVariables = Object.keys(variables).length > 0;
  return (
    <BaseModal
      open={datasetCase !== undefined}
      variant="complex"
      data-testid="dataset-case-details"
      title={t('features.agentEvaluation.cases.detailsTitle', 'Case details')}
      onClose={onClose}
      content={
        datasetCase !== undefined && (
          <Box sx={detailsSx}>
            <TextField
              fullWidth
              multiline
              disabled
              label={t('features.agentEvaluation.cases.inputLabel', 'What the agent is asked')}
              value={datasetCase.input}
              slotProps={{ htmlInput: { 'data-testid': 'dataset-case-details-input' } }}
            />
            <TextField
              fullWidth
              multiline
              disabled
              label={t('features.agentEvaluation.cases.expectedLabel', 'Expected answer (optional)')}
              value={datasetCase.expected_output ?? ''}
              placeholder={t('features.agentEvaluation.cases.noExpected', 'No expected answer stated')}
              slotProps={{
                inputLabel: { shrink: true },
                htmlInput: { 'data-testid': 'dataset-case-details-expected' },
              }}
            />
            {hasVariables && (
              <TextField
                fullWidth
                multiline
                disabled
                label={t('features.agentEvaluation.cases.variablesLabel', 'Variables')}
                value={JSON.stringify(variables, null, 2)}
                slotProps={{ htmlInput: { 'data-testid': 'dataset-case-details-variables' } }}
              />
            )}
          </Box>
        )
      }
    />
  );
}

interface DatasetCaseRowProps {
  readonly datasetCase: EvalDatasetCase;
  readonly canEdit: boolean;
  readonly isToggling: boolean;
  /** `true` excludes the case, `false` includes it again. */
  readonly onToggle: (excluded: boolean) => void;
  readonly onRemove: () => void;
  readonly onViewDetails: () => void;
}

function DatasetCaseRow(props: DatasetCaseRowProps): ReactNode {
  const { datasetCase, canEdit, isToggling, onToggle, onRemove, onViewDetails } = props;
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
      <Box sx={caseTextSx} data-excluded={isExcluded ? 'true' : undefined}>
        <Typography
          variant="bodyMedium"
          sx={isExcluded ? excludedInputSx : undefined}
          data-testid={`dataset-case-input-text-${datasetCase.id}`}
        >
          {datasetCase.input}
        </Typography>
        {/*
          An absent expected answer is SAID so, rather than left blank. A
          blank cell reads as "the author wrote nothing here", which is the
          same thing an empty expected answer would look like — and the two
          mean different things to the judge.
        */}
        <Typography
          variant="bodySmall"
          color={isExcluded ? undefined : 'text.secondary'}
          sx={isExcluded ? excludedExpectedSx : undefined}
          data-testid={`dataset-case-expected-text-${datasetCase.id}`}
        >
          {datasetCase.expected_output === null || datasetCase.expected_output === undefined
            ? t('features.agentEvaluation.cases.noExpected', 'No expected answer stated')
            : t('features.agentEvaluation.cases.expected', 'Expected: {{answer}}', {
                answer: datasetCase.expected_output,
              })}
        </Typography>
      </Box>
      <BaseBtn
        variant="tertiary"
        className={VIEW_DETAILS_CLASS}
        data-testid={`dataset-case-view-details-${datasetCase.id}`}
        onClick={onViewDetails}
      >
        {t('features.agentEvaluation.cases.viewDetails', 'View details')}
      </BaseBtn>
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
  const [togglingIds, setTogglingIds] = useState<ReadonlySet<string>>(() => new Set());
  // Keyed by case id. The mutation's own `.error` is the LATEST call's, so a
  // second toggle would wipe the first one's refusal before anyone read it.
  const [toggleErrors, setToggleErrors] = useState<ReadonlyMap<string, string>>(() => new Map());
  const [detailsCaseId, setDetailsCaseId] = useState<string | undefined>(undefined);

  const detail = detailQuery.data;
  const cases = detail?.cases ?? [];
  const activeCount = cases.filter((datasetCase) => !datasetCase.excluded).length;
  const addError = datasetErrorMessage(mutations.addCase.error);
  // The toggle and the remove button fail SILENTLY without these: there is
  // no optimistic update, so a refused PUT or DELETE (no dataset.update, a
  // case another tab deleted, a network failure) just leaves the row as it
  // was, and the author cannot tell why the click did nothing.
  const toggleError = toggleErrors.size > 0 ? [...new Set(toggleErrors.values())].join(' ') : undefined;
  const removeError = datasetErrorMessage(mutations.removeCase.error);

  const handleToggle = (caseId: string, excluded: boolean): void => {
    setTogglingIds((current) => new Set(current).add(caseId));
    mutations.setCaseExcluded
      .mutateAsync({ datasetId, caseId, excluded })
      .then(
        () =>
          setToggleErrors((current) => {
            if (!current.has(caseId)) return current;
            const next = new Map(current);
            next.delete(caseId);
            return next;
          }),
        (error: unknown) =>
          setToggleErrors((current) => new Map(current).set(caseId, datasetErrorMessage(error) ?? '')),
      )
      .finally(() =>
        setTogglingIds((current) => {
          const next = new Set(current);
          next.delete(caseId);
          return next;
        }),
      );
  };

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
          isToggling={togglingIds.has(datasetCase.id)}
          onToggle={(excluded) => handleToggle(datasetCase.id, excluded)}
          onRemove={() => mutations.removeCase.mutate({ datasetId, caseId: datasetCase.id })}
          onViewDetails={() => setDetailsCaseId(datasetCase.id)}
        />
      ))}

      <CaseDetailsModal
        datasetCase={cases.find((datasetCase) => datasetCase.id === detailsCaseId)}
        onClose={() => setDetailsCaseId(undefined)}
      />

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
