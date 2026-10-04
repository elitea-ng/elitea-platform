/**
 * The dimension editor's field layout.
 *
 * Split from `DimensionEditorDialog` for the §3.5 file-length and
 * cyclomatic-complexity budgets: the dialog owns state, saving and error
 * reporting; this owns what is on screen.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { InputBase } from '@/shared/ui/InputBase';
import { SingleSelect } from '@/shared/ui/SingleSelect';
import type { SingleSelectOption } from '@/shared/ui/SingleSelectMenuItem';

import { isCodeOnly, MAX_DIMENSION_NAME_LENGTH } from '../lib/dimensionForm';
import {
  EVAL_ENGINE,
  EVAL_POLARITY,
  EVAL_RETURN_CONTRACT,
  EVAL_SCALE_TYPE,
  EVAL_TARGET_OPERATORS,
  EVAL_TIER,
  type EvalDimensionForm,
  type EvalEngine,
} from '../model/types';

const columnSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '1rem',
};
const rowSx: SxProps<Theme> = { display: 'flex', gap: '1rem' };
const engineRowSx: SxProps<Theme> = { display: 'flex', gap: '1.5rem' };
const fieldSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
};

function engineOptions(): readonly {
  readonly value: EvalEngine;
  readonly label: string;
}[] {
  return [
    {
      value: EVAL_ENGINE.ai,
      label: t('features.agentEvaluation.engine.ai', 'AI'),
    },
    {
      value: EVAL_ENGINE.human,
      label: t('features.agentEvaluation.engine.human', 'Human'),
    },
    {
      value: EVAL_ENGINE.code,
      label: t('features.agentEvaluation.engine.code', 'Code'),
    },
  ];
}

function scaleTypeOptions(): SingleSelectOption[] {
  return [
    {
      value: EVAL_SCALE_TYPE.continuous,
      label: t('features.agentEvaluation.scale.continuous', 'Continuous (e.g. 0-100)'),
    },
    {
      value: EVAL_SCALE_TYPE.ordinal,
      label: t('features.agentEvaluation.scale.ordinal', 'Ordinal (e.g. 1-5)'),
    },
    {
      value: EVAL_SCALE_TYPE.binary,
      label: t('features.agentEvaluation.scale.binary', 'Binary (0/1)'),
    },
  ];
}

function polarityOptions(): SingleSelectOption[] {
  return [
    {
      value: EVAL_POLARITY.higherBetter,
      label: t('features.agentEvaluation.polarity.higher', 'Higher is better'),
    },
    {
      value: EVAL_POLARITY.lowerBetter,
      label: t('features.agentEvaluation.polarity.lower', 'Lower is better'),
    },
  ];
}

/**
 * The scope options.
 *
 * On an EDIT the scope can move one way only: an agent dimension can become a
 * project dimension (legacy issue 6669), and the server refuses the reverse
 * move with a 409, because other agents can already use a project dimension.
 * So "This agent only" is disabled for a stored project dimension, and the
 * select never offers a save that always fails.
 */
function tierOptions(canScopeToAgent: boolean, storedTier?: EvalDimensionForm['tier']): SingleSelectOption[] {
  const isStoredProject = storedTier === EVAL_TIER.project;
  return [
    {
      value: EVAL_TIER.agentAdhoc,
      label: t('features.agentEvaluation.tier.agent', 'This agent only'),
      disabled: !canScopeToAgent || isStoredProject,
    },
    {
      value: EVAL_TIER.project,
      label: t('features.agentEvaluation.tier.project', 'Project library'),
    },
  ];
}

function returnContractOptions(): SingleSelectOption[] {
  return [
    {
      value: EVAL_RETURN_CONTRACT.bool,
      label: t('features.agentEvaluation.return.bool', 'Boolean (pass / fail)'),
    },
    {
      value: EVAL_RETURN_CONTRACT.number,
      label: t('features.agentEvaluation.return.number', 'Number (score)'),
    },
  ];
}

function operatorOptions(): SingleSelectOption[] {
  return EVAL_TARGET_OPERATORS.map((operator) => ({
    value: operator,
    label: operator,
  }));
}

export interface DimensionEditorFieldsProps {
  readonly form: EvalDimensionForm;
  readonly isEdit: boolean;
  readonly canScopeToAgent: boolean;
  /** The tier the stored dimension has. `undefined` in create mode. */
  readonly storedTier: EvalDimensionForm['tier'] | undefined;
  readonly onFieldChange: <K extends keyof EvalDimensionForm>(key: K, value: EvalDimensionForm[K]) => void;
  readonly onToggleEngine: (engine: EvalEngine) => void;
}

export function DimensionEditorFields(props: DimensionEditorFieldsProps): ReactNode {
  const { form, isEdit, canScopeToAgent, storedTier, onFieldChange, onToggleEngine } = props;
  const isCode = isCodeOnly(form.allowed_engines);

  return (
    <Box sx={columnSx}>
      <InputBase
        data-testid="dimension-name-input"
        fullWidth
        label={t('features.agentEvaluation.field.name', 'Name')}
        value={form.name}
        slotProps={{ htmlInput: { maxLength: MAX_DIMENSION_NAME_LENGTH } }}
        onChange={(event) => onFieldChange('name', event.target.value)}
      />
      <InputBase
        data-testid="dimension-rubric-input"
        fullWidth
        expand={{ minRows: isCode ? 2 : 4 }}
        label={
          isCode
            ? t('features.agentEvaluation.field.description', 'Description')
            : t(
                'features.agentEvaluation.field.rubric',
                'Rubric / description (used as the AI grading prompt)',
              )
        }
        value={form.description}
        onChange={(event) => onFieldChange('description', event.target.value)}
      />

      {/*
        Scope is editable on an edit too. The update sends the tier the
        author picked, and the server promotes an agent dimension to the
        project library on request. A move back is refused, so the option is
        disabled for a stored project dimension (see `tierOptions`).
      */}
      <SingleSelect
        label={t('features.agentEvaluation.field.scope', 'Scope')}
        value={form.tier}
        options={tierOptions(canScopeToAgent, isEdit ? storedTier : undefined)}
        onChange={(value) => onFieldChange('tier', value as EvalDimensionForm['tier'])}
        id="dimension-tier-select"
      />

      <Box sx={fieldSx}>
        <Typography variant="bodyMedium" component="p">{t('features.agentEvaluation.field.engines', 'Engines')}</Typography>
        <Box sx={engineRowSx}>
          {engineOptions().map((option) => (
            <FormControlLabel
              key={option.value}
              label={option.label}
              control={
                <BaseCheckbox
                  checked={form.allowed_engines.includes(option.value)}
                  onChange={() => onToggleEngine(option.value)}
                  data-testid={`dimension-engine-${option.value}`}
                />
              }
            />
          ))}
        </Box>
      </Box>

      {isCode && (
        <>
          <InputBase
            data-testid="dimension-code-input"
            fullWidth
            expand={{ minRows: 6 }}
            label={t('features.agentEvaluation.field.code', 'Validation code (Python)')}
            value={form.code}
            onChange={(event) => onFieldChange('code', event.target.value)}
          />
          <SingleSelect
            label={t('features.agentEvaluation.field.returnContract', 'Return contract')}
            value={form.return_contract}
            options={returnContractOptions()}
            onChange={(value) =>
              onFieldChange('return_contract', value as EvalDimensionForm['return_contract'])
            }
            id="dimension-return-contract-select"
          />
        </>
      )}

      <SingleSelect
        label={t('features.agentEvaluation.field.scaleType', 'Scale type')}
        value={form.scale_type}
        options={scaleTypeOptions()}
        onChange={(value) => onFieldChange('scale_type', value as EvalDimensionForm['scale_type'])}
        id="dimension-scale-type-select"
      />

      <Box sx={rowSx}>
        <InputBase
          data-testid="dimension-scale-min-input"
          fullWidth
          type="number"
          label={t('features.agentEvaluation.field.scaleMin', 'Scale min')}
          value={form.scale_min}
          onChange={(event) => onFieldChange('scale_min', event.target.value)}
        />
        <InputBase
          data-testid="dimension-scale-max-input"
          fullWidth
          type="number"
          label={t('features.agentEvaluation.field.scaleMax', 'Scale max')}
          value={form.scale_max}
          onChange={(event) => onFieldChange('scale_max', event.target.value)}
        />
      </Box>

      {/*
        No placeholder value and no default. Polarity is applied last in score
        normalisation, so an inverse metric authored without it scores a good
        answer 0 — silently. The author has to say which direction is good.
      */}
      <SingleSelect
        label={t('features.agentEvaluation.field.polarity', 'Polarity')}
        placeholder={t('features.agentEvaluation.field.polarityPlaceholder', 'Choose a direction')}
        value={form.polarity}
        options={polarityOptions()}
        onChange={(value) => onFieldChange('polarity', value as EvalDimensionForm['polarity'])}
        id="dimension-polarity-select"
      />

      <InputBase
        data-testid="dimension-weight-input"
        fullWidth
        type="number"
        label={t('features.agentEvaluation.field.weight', 'Default weight')}
        value={form.default_weight}
        onChange={(event) => onFieldChange('default_weight', event.target.value)}
      />

      <Box sx={rowSx}>
        <SingleSelect
          label={t('features.agentEvaluation.field.targetOperator', 'Default target operator')}
          placeholder={t('features.agentEvaluation.field.targetOperatorPlaceholder', 'None')}
          value={form.default_target_operator}
          options={operatorOptions()}
          onChange={(value) =>
            onFieldChange('default_target_operator', value as EvalDimensionForm['default_target_operator'])
          }
          onClear={() => onFieldChange('default_target_operator', '')}
          id="dimension-target-operator-select"
        />
        <InputBase
          data-testid="dimension-target-input"
          fullWidth
          type="number"
          label={t('features.agentEvaluation.field.target', 'Default target')}
          value={form.default_target}
          onChange={(event) => onFieldChange('default_target', event.target.value)}
        />
      </Box>
    </Box>
  );
}
