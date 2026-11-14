/**
 * The run limits editor: token and cost limits per case and per run, and what a
 * run does when its run limit is reached.
 *
 * WHAT A "SUITE" IS HERE. A named set of those settings. A run started with one
 * copies the settings at start, so editing or deleting a suite changes no run
 * that already started. The limits are enforced by the server from the usage
 * the gateway attributed to the run; they are checked between model calls, so a
 * run can pass a limit by the call that crosses it.
 *
 * ONLY THE CONTROLS THAT DO SOMETHING. Steps, time and tool output limits are
 * not offered: a case is one model call with no tools today, so they would
 * change nothing.
 *
 * Without the right to start runs the form is read-only: it shows the limits and
 * offers no save.
 */
import { useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import Radio from '@mui/material/Radio';
import RadioGroup from '@mui/material/RadioGroup';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { EvalSuite } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { suiteErrorMessage } from '../lib/evaluationError';
import {
  EMPTY_SUITE_FORM,
  suiteFormError,
  suiteFormToInput,
  suiteToForm,
  type RunLimitAction,
  type SuiteForm,
} from '../lib/suiteForm';
import { useEvalSuiteMutations } from '../model/useEvalSuites';

const panelSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  padding: '0.75rem',
  border: 1,
  borderColor: 'divider',
  borderRadius: 'var(--el-shape-radiusSm, 4px)',
};

/** The select value of "a new suite". */
const NEW_SUITE = '';

interface RunSuitesPanelProps {
  readonly projectId: string | undefined;
  readonly suites: readonly EvalSuite[];
  readonly canEdit: boolean;
}

interface LimitFieldProps {
  readonly label: string;
  readonly value: string;
  readonly testId: string;
  readonly disabled: boolean;
  readonly onChange: (value: string) => void;
}

function LimitField(props: LimitFieldProps): ReactNode {
  const { label, value, testId, disabled, onChange } = props;
  return (
    <TextField
      size="small"
      label={label}
      value={value}
      disabled={disabled}
      helperText={t('features.agentEvaluation.suites.emptyIsNoLimit', 'Empty means no limit.')}
      slotProps={{ htmlInput: { 'data-testid': testId, inputMode: 'decimal' } }}
      onChange={(event) => onChange(event.target.value)}
    />
  );
}

export function RunSuitesPanel(props: RunSuitesPanelProps): ReactNode {
  const { projectId, suites, canEdit } = props;
  const [selectedId, setSelectedId] = useState(NEW_SUITE);
  const [form, setForm] = useState<SuiteForm>(EMPTY_SUITE_FORM);
  const mutations = useEvalSuiteMutations(projectId);

  const formError = suiteFormError(form);
  const serverError = suiteErrorMessage(mutations.save.error ?? mutations.remove.error);
  const set = (patch: Partial<SuiteForm>): void => setForm((previous) => ({ ...previous, ...patch }));

  const choose = (id: string): void => {
    setSelectedId(id);
    const suite = suites.find((entry) => entry.id === id);
    setForm(suite === undefined ? EMPTY_SUITE_FORM : suiteToForm(suite));
    mutations.save.reset();
    mutations.remove.reset();
  };

  const save = (): void => {
    if (formError !== undefined) return;
    mutations.save.mutate(
      { suiteId: selectedId === NEW_SUITE ? undefined : selectedId, input: suiteFormToInput(form) },
      { onSuccess: (saved) => setSelectedId(saved.id) },
    );
  };

  const remove = (): void => {
    mutations.remove.mutate(selectedId, { onSuccess: () => choose(NEW_SUITE) });
  };

  return (
    <Box sx={panelSx} data-testid="run-suites-panel">
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'features.agentEvaluation.suites.explainer',
          'Limits are checked between model calls from the usage already recorded for the run, so a run can pass a limit by the call that crosses it. Tokens are prompt plus completion tokens; costs are estimates from the price list.',
        )}
      </Typography>

      <TextField
        select
        size="small"
        label={t('features.agentEvaluation.suites.chooseLabel', 'Saved limits')}
        value={selectedId}
        slotProps={{ htmlInput: { 'data-testid': 'suite-select' } }}
        onChange={(event) => choose(event.target.value)}
      >
        <MenuItem value={NEW_SUITE}>{t('features.agentEvaluation.suites.newOption', 'New limits')}</MenuItem>
        {suites.map((suite) => (
          <MenuItem key={suite.id} value={suite.id}>
            {suite.name}
          </MenuItem>
        ))}
      </TextField>

      <TextField
        size="small"
        label={t('features.agentEvaluation.suites.nameLabel', 'Name')}
        value={form.name}
        disabled={!canEdit}
        slotProps={{ htmlInput: { 'data-testid': 'suite-name', maxLength: 128 } }}
        onChange={(event) => set({ name: event.target.value })}
      />
      <LimitField
        label={t('features.agentEvaluation.suites.tokensPerCase', 'Tokens per case')}
        value={form.tokenLimitPerCase}
        testId="suite-tokens-per-case"
        disabled={!canEdit}
        onChange={(value) => set({ tokenLimitPerCase: value })}
      />
      <LimitField
        label={t('features.agentEvaluation.suites.costPerCase', 'Cost per case (USD)')}
        value={form.costLimitPerCase}
        testId="suite-cost-per-case"
        disabled={!canEdit}
        onChange={(value) => set({ costLimitPerCase: value })}
      />
      <LimitField
        label={t('features.agentEvaluation.suites.tokensPerRun', 'Tokens per run')}
        value={form.tokenLimitPerRun}
        testId="suite-tokens-per-run"
        disabled={!canEdit}
        onChange={(value) => set({ tokenLimitPerRun: value })}
      />
      <LimitField
        label={t('features.agentEvaluation.suites.costPerRun', 'Cost per run (USD)')}
        value={form.costLimitPerRun}
        testId="suite-cost-per-run"
        disabled={!canEdit}
        onChange={(value) => set({ costLimitPerRun: value })}
      />

      <Typography variant="bodySmall" color="text.secondary">
        {t('features.agentEvaluation.suites.whenReached', 'When the run limit is reached')}
      </Typography>
      <RadioGroup
        value={form.onRunLimit}
        onChange={(event) => set({ onRunLimit: event.target.value as RunLimitAction })}
      >
        <FormControlLabel
          value="stop"
          disabled={!canEdit}
          control={<Radio />}
          label={t('features.agentEvaluation.suites.stop', 'Stop the run')}
        />
        <FormControlLabel
          value="report"
          disabled={!canEdit}
          control={<Radio />}
          label={t('features.agentEvaluation.suites.report', 'Only report it')}
        />
      </RadioGroup>

      {canEdit && (
        <Box sx={{ display: 'flex', gap: '0.5rem' }}>
          <BaseBtn
            variant="contained"
            data-testid="suite-save"
            disabled={formError !== undefined || mutations.save.isPending}
            onClick={save}
          >
            {t('features.agentEvaluation.suites.save', 'Save limits')}
          </BaseBtn>
          {selectedId !== NEW_SUITE && (
            <BaseBtn variant="text" data-testid="suite-delete" disabled={mutations.remove.isPending} onClick={remove}>
              {t('features.agentEvaluation.suites.delete', 'Delete')}
            </BaseBtn>
          )}
        </Box>
      )}

      {(serverError ?? (canEdit ? formError : undefined)) !== undefined && (
        <Typography role="alert" variant="bodyMedium" component="p" color="error" data-testid="suite-error">
          {serverError ?? formError}
        </Typography>
      )}
    </Box>
  );
}
