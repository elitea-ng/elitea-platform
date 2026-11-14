/**
 * The execution mode of a suite and the controls that belong to its agent-run
 * mode (issue 2013, step 2): a steps limit and a time limit per case, and
 * whether the stored evidence keeps tool outputs.
 *
 * THE THREE CONTROLS ARE SHOWN ONLY FOR `runtime_plane`. A case of the default
 * mode is one model call with no steps and no tools, so a steps limit or a tool
 * outputs setting would change nothing, and showing it would mislead.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import type { ExecutionMode, SuiteForm } from '../lib/suiteForm';

export interface SuiteRuntimeControlsProps {
  readonly form: SuiteForm;
  readonly disabled: boolean;
  readonly onChange: (patch: Partial<SuiteForm>) => void;
}

function WholeField(props: {
  readonly label: string;
  readonly helper: string;
  readonly value: string;
  readonly testId: string;
  readonly disabled: boolean;
  readonly onChange: (value: string) => void;
}): ReactNode {
  const { label, helper, value, testId, disabled, onChange } = props;
  return (
    <TextField
      size="small"
      label={label}
      value={value}
      disabled={disabled}
      helperText={helper}
      slotProps={{ htmlInput: { 'data-testid': testId, inputMode: 'numeric' } }}
      onChange={(event) => onChange(event.target.value)}
    />
  );
}

export function SuiteRuntimeControls({ form, disabled, onChange }: SuiteRuntimeControlsProps): ReactNode {
  const runtime = form.executionMode === 'runtime_plane';
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }} data-testid="suite-execution">
      <TextField
        select
        size="small"
        label={t('features.agentEvaluation.suites.executionMode', 'How a case runs')}
        value={form.executionMode}
        disabled={disabled}
        slotProps={{ htmlInput: { 'data-testid': 'suite-execution-mode' } }}
        onChange={(event) => onChange({ executionMode: event.target.value as ExecutionMode })}
      >
        <MenuItem value="predict_blocking">
          {t('features.agentEvaluation.suites.modeModelCall', 'One model call (instructions and model only)')}
        </MenuItem>
        <MenuItem value="runtime_plane">
          {t('features.agentEvaluation.suites.modeAgentRun', 'Agent run (tools, steps and trajectory)')}
        </MenuItem>
      </TextField>
      {runtime && (
        <>
          <Typography variant="bodySmall" color="text.secondary" data-testid="suite-runtime-note">
            {t(
              'features.agentEvaluation.suites.runtimeNote',
              'Each case runs the pinned agent version on the worker with its tools, under the same controls as a chat. A run is unattended: a step that needs a person’s approval fails the case.',
            )}
          </Typography>
          <WholeField
            label={t('features.agentEvaluation.suites.stepsPerCase', 'Steps per case')}
            helper={t('features.agentEvaluation.suites.stepsHelp', 'Model calls and tool calls. The run is cancelled when it passes this. Empty means no limit.')}
            value={form.stepLimitPerCase}
            testId="suite-steps-per-case"
            disabled={disabled}
            onChange={(value) => onChange({ stepLimitPerCase: value })}
          />
          <WholeField
            label={t('features.agentEvaluation.suites.secondsPerCase', 'Time per case (seconds)')}
            helper={t('features.agentEvaluation.suites.timeHelp', 'The run is cancelled at the deadline and the case stores a timeout. Empty means 15 minutes.')}
            value={form.timeLimitPerCaseSeconds}
            testId="suite-time-per-case"
            disabled={disabled}
            onChange={(value) => onChange({ timeLimitPerCaseSeconds: value })}
          />
          <FormControlLabel
            control={
              <BaseCheckbox
                checked={form.storeToolOutputs}
                disabled={disabled}
                data-testid="suite-store-tool-outputs"
                onChange={(event) => onChange({ storeToolOutputs: event.target.checked })}
              />
            }
            label={t('features.agentEvaluation.suites.storeToolOutputs', 'Store tool outputs in the results')}
          />
        </>
      )}
    </Box>
  );
}
