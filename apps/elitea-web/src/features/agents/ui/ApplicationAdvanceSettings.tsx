import type { ChangeEvent, KeyboardEvent, ReactNode } from 'react';
import { useCallback } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import type { OutputContract } from '@/shared/api/generated/model';
import { MAX_STEP_LIMIT, MIN_STEP_LIMIT } from '@/shared/lib/limits';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';
import { OutputContractField, readOutputContract } from '@/shared/ui/OutputContractField';
import { StyledInputEnhancer } from '@/shared/ui/StyledInputEnhancer';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent/ui/agent-details/
 * configurations/ApplicationAdvanceSettings.jsx`.
 *
 * DISCLOSED REDESIGN — no ambient form context (see `../model/types.ts`'s
 * module doc comment): `stepLimit`/`ignoreProjectContext` are explicit
 * props instead of `useFormikContext()` reads, and the two setters replace
 * `formik.setFieldValue('version_details.meta.step_limit', ...)`/
 * `'...ignore_project_context'` calls.
 *
 * `showMode={AccordionConstants.AccordionShowMode.LeftMode}` -> `showMode="left"`
 * (this port's `BasicAccordion` uses a plain string union, not the baseline's
 * object-valued enum — see `shared/ui/BasicAccordion.tsx`'s own type).
 */
export interface ApplicationAdvanceSettingsProps {
  readonly stepLimit: number | undefined;
  readonly onStepLimitChange: (value: number | undefined) => void;
  readonly showIgnoreProjectContext?: boolean | undefined;
  readonly ignoreProjectContext?: boolean | undefined;
  readonly onIgnoreProjectContextChange?: ((checked: boolean) => void) | undefined;
  readonly disabled?: boolean | undefined;
  /**
   * The model picker, injected by the page (`widgets/agent-model-settings`)
   * — `.dependency-cruiser.cjs` forbids `features/` importing `widgets/`.
   * Rendered above the step limit because the two belong to the same panel
   * in the baseline, and because the picker's own settings dialog
   * deliberately withholds a second step-limit input: that field is the one
   * below, and it is this component's.
   */
  readonly modelSettingsSlot?: ReactNode | undefined;
  /**
   * #1500 — `meta.output_contract` (ADR-0043 decision 2). The field shows only
   * with `showOutputContract`: only the agent create and edit pages route the
   * path to a save today, so only they opt in (through `CreateAgentForm`'s
   * prop of the same name). The stored value is opaque `meta` content, read here.
   */
  readonly outputContract?: unknown;
  readonly showOutputContract?: boolean | undefined;
  readonly onOutputContractChange?: ((next: OutputContract | null) => void) | undefined;
  readonly sx?: SxProps<Theme> | undefined;
}

function clampStepLimit(rawValue: string): number | undefined {
  if (rawValue === '') return undefined;
  const numericValue = Number.parseInt(rawValue, 10);
  if (Number.isNaN(numericValue)) return undefined;
  if (numericValue > MAX_STEP_LIMIT) return MAX_STEP_LIMIT;
  if (numericValue < MIN_STEP_LIMIT) return MIN_STEP_LIMIT;
  return numericValue;
}

const NAVIGATION_KEYS = new Set([
  'Backspace',
  'Delete',
  'Tab',
  'Escape',
  'Enter',
  'ArrowLeft',
  'ArrowRight',
  'ArrowUp',
  'ArrowDown',
  'Home',
  'End',
]);

function isValidKeyInput(key: string, currentValue: string, allowedModifiers: boolean): boolean {
  if (allowedModifiers) return true;
  if (NAVIGATION_KEYS.has(key)) return true;
  if (/[0-9]/.test(key)) {
    const nextValue = `${currentValue}${key}`;
    return Number.parseInt(nextValue, 10) <= MAX_STEP_LIMIT;
  }
  return false;
}

export function ApplicationAdvanceSettings({
  stepLimit,
  onStepLimitChange,
  showIgnoreProjectContext = false,
  ignoreProjectContext = false,
  onIgnoreProjectContextChange,
  disabled,
  modelSettingsSlot,
  outputContract,
  onOutputContractChange,
  showOutputContract,
  sx,
}: ApplicationAdvanceSettingsProps): ReactNode {
  const handleChange = useCallback(
    (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      onStepLimitChange(clampStepLimit(event.target.value));
    },
    [onStepLimitChange],
  );

  const handleKeyDown = useCallback((event: KeyboardEvent<HTMLDivElement>) => {
    const currentValue = (event.target as HTMLInputElement).value;
    if (!isValidKeyInput(event.key, currentValue, event.ctrlKey || event.metaKey)) {
      event.preventDefault();
    }
  }, []);

  const handleIgnoreToggle = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => {
      onIgnoreProjectContextChange?.(event.target.checked);
    },
    [onIgnoreProjectContextChange],
  );

  // A plain array, not a `useMemo`: the dependency list passed the §3.5
  // hook-deps budget (8) when the output contract joined it, and the React
  // compiler memoises this render anyway.
  const accordionItems = [
    {
      title: t('features.agents.applicationAdvanceSettings.title', 'Advanced'),
      content: (
        <Box sx={fieldContainerSx}>
          {modelSettingsSlot}
          <StyledInputEnhancer
            value={stepLimit ?? ''}
            onChange={handleChange}
            onKeyDown={handleKeyDown}
            disabled={disabled}
            label={
              <InfoLabelWithTooltip
                label={t('features.agents.applicationAdvanceSettings.stepLimit.label', 'Step limit')}
                tooltip={t(
                  'features.agents.applicationAdvanceSettings.stepLimit.tooltip',
                  'The maximum number of steps to take before ending the execution loop (tools call limit).',
                )}
                variant="bodyMedium"
              />
            }
            type="text"
            slotProps={{
              htmlInput: { inputMode: 'numeric', pattern: '[0-9]*', min: MIN_STEP_LIMIT, max: MAX_STEP_LIMIT },
            }}
          />
          {showOutputContract === true && onOutputContractChange !== undefined && (
            <OutputContractField
              value={readOutputContract(outputContract)}
              onChange={onOutputContractChange}
              disabled={disabled}
              testIdPrefix="agent-output-contract"
            />
          )}
          {showIgnoreProjectContext && (
            <Box sx={toggleRowSx}>
              <BaseCheckbox
                checked={ignoreProjectContext}
                onChange={handleIgnoreToggle}
                disabled={disabled}
              />
              <InfoLabelWithTooltip
                label={t('features.agents.applicationAdvanceSettings.ignoreProjectContext.label', 'Ignore Project Context')}
                tooltip={t(
                  'features.agents.applicationAdvanceSettings.ignoreProjectContext.tooltip',
                  'When enabled, this agent will not use the project background context in its responses.',
                )}
                variant="bodyMedium"
                sx={labelSx}
              />
            </Box>
          )}
        </Box>
      ),
    },
  ];

  return (
    <BasicAccordion
      showMode="left"
      slotSx={{ accordion: accordionSx, ...(sx !== undefined ? { root: sx } : {}) }}
      items={accordionItems}
    />
  );
}

const accordionSx: SxProps<Theme> = (theme: Theme) => ({
  background: theme.vars.palette.background.tabPanel,
});

const fieldContainerSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.75rem',
  marginTop: '0.5rem',
};

const toggleRowSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
};

const labelSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.secondary,
});
