/**
 * The three proposal steps of the Enhance with AI wizard: instructions,
 * rubrics and targets, expected outputs. Each lists what the model proposed
 * with its rationale and a checkbox to accept it. Accepting saves nothing: the
 * review step does that, through the normal routes.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type {
  EvalEnhanceDatasetChange,
  EvalEnhanceDimensionChange,
  EvalEnhanceProposal,
} from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { lineDiff } from '@/shared/lib/lineDiff';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { DiffView } from '@/shared/ui/DiffView';

const itemSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
  padding: '0.5rem 0',
  borderBottom: (theme) => `1px solid ${theme.vars.palette.divider}`,
};

function Nothing({ text }: { readonly text: string }): ReactNode {
  return (
    <Typography component="output" variant="bodyMedium" color="text.secondary" data-testid="enhance-nothing">
      {text}
    </Typography>
  );
}

function Accept({ checked, label, onChange }: { readonly checked: boolean; readonly label: string; readonly onChange: (next: boolean) => void }): ReactNode {
  return (
    <FormControlLabel
      label={label}
      control={
        <BaseCheckbox checked={checked} onChange={(event) => onChange(event.target.checked)} />
      }
    />
  );
}

export interface InstructionStepProps {
  readonly proposal: EvalEnhanceProposal;
  /** The pinned version's current instructions; `undefined` when they could not be read. */
  readonly current: string | undefined;
  readonly accepted: boolean;
  readonly onChange: (next: boolean) => void;
}

export function InstructionStep({ proposal, current, accepted, onChange }: InstructionStepProps): ReactNode {
  const patch = proposal.instruction_patch;
  if (patch === null || patch === undefined) {
    return <Nothing text={t('features.agentEvaluation.enhance.noInstructions', 'No instruction change proposed.')} />;
  }
  return (
    <Box sx={itemSx} data-testid="enhance-instruction">
      <Typography variant="bodyMedium">{patch.rationale}</Typography>
      {current === undefined ? (
        <Typography component="pre" variant="bodySmall" data-testid="enhance-instruction-text">
          {patch.instructions}
        </Typography>
      ) : (
        <DiffView parts={lineDiff(current, patch.instructions)} data-testid="enhance-instruction-diff" />
      )}
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'features.agentEvaluation.enhance.instructionsNote',
          'Accepting creates a new draft version of the agent. Publishing it follows the agent’s normal release path, including a change request for agents above low risk.',
        )}
      </Typography>
      <Accept
        checked={accepted}
        label={t('features.agentEvaluation.enhance.acceptInstructions', 'Create a draft version with these instructions')}
        onChange={onChange}
      />
    </Box>
  );
}

function describeDimension(change: EvalEnhanceDimensionChange): string {
  const parts: string[] = [];
  if (change.description !== undefined) {
    parts.push(t('features.agentEvaluation.enhance.newRubric', 'New rubric: {{text}}', { text: change.description }));
  }
  if (change.default_target !== undefined) {
    parts.push(
      t('features.agentEvaluation.enhance.newTarget', 'New target: {{operator}} {{target}}', {
        operator: change.default_target_operator ?? '',
        target: change.default_target,
      }),
    );
  }
  return parts.join(' · ');
}

export interface DimensionStepProps {
  readonly changes: readonly EvalEnhanceDimensionChange[];
  readonly accepted: ReadonlySet<string>;
  readonly onToggle: (id: string, next: boolean) => void;
}

export function DimensionStep({ changes, accepted, onToggle }: DimensionStepProps): ReactNode {
  if (changes.length === 0) {
    return <Nothing text={t('features.agentEvaluation.enhance.noDimensions', 'No rubric or target change proposed.')} />;
  }
  return (
    <>
      {changes.map((change) => (
        <Box key={change.dimension_id} sx={itemSx} data-testid={`enhance-dimension-${change.dimension_id}`}>
          <Typography variant="labelMedium">{change.dimension_name}</Typography>
          <Typography variant="bodyMedium">{describeDimension(change)}</Typography>
          <Typography variant="bodySmall" color="text.secondary">
            {change.rationale}
          </Typography>
          <Accept
            checked={accepted.has(change.dimension_id)}
            label={t('features.agentEvaluation.enhance.acceptChange', 'Apply this change')}
            onChange={(next) => onToggle(change.dimension_id, next)}
          />
        </Box>
      ))}
      <Typography variant="bodySmall" color="text.secondary">
        {t('features.agentEvaluation.enhance.dimensionNote', 'A changed dimension applies to new runs. This run keeps the rubric it was scored with.')}
      </Typography>
    </>
  );
}

export interface CaseStepProps {
  readonly changes: readonly EvalEnhanceDatasetChange[];
  readonly accepted: ReadonlySet<string>;
  readonly onToggle: (id: string, next: boolean) => void;
}

export function CaseStep({ changes, accepted, onToggle }: CaseStepProps): ReactNode {
  if (changes.length === 0) {
    return <Nothing text={t('features.agentEvaluation.enhance.noCases', 'No expected-output change proposed.')} />;
  }
  return (
    <>
      {changes.map((change) => (
        <Box key={change.case_id} sx={itemSx} data-testid={`enhance-case-${change.case_id}`}>
          <Typography variant="labelMedium">
            {t('features.agentEvaluation.scorecard.caseLabel', 'Case {{id}}', { id: change.case_id })}
          </Typography>
          <Typography variant="bodySmall" color="text.secondary">
            {change.input}
          </Typography>
          <DiffView parts={lineDiff(change.current_expected_output ?? '', change.expected_output)} />
          <Typography variant="bodySmall" color="text.secondary">
            {change.rationale}
          </Typography>
          <Accept
            checked={accepted.has(change.case_id)}
            label={t('features.agentEvaluation.enhance.acceptChange', 'Apply this change')}
            onChange={(next) => onToggle(change.case_id, next)}
          />
        </Box>
      ))}
    </>
  );
}
