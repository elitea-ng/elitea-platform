/**
 * The composer footer's left side: Act | Plan (| Bypass, for a local
 * thread), the agent·version picker, the model, where the thread runs and
 * the meters.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';

import { t } from '@/shared/i18n';
import { Segmented } from '@/widgets/desktop-shell';

interface FooterControlsProps {
  planMode: boolean;
  onPlanModeChange: (planMode: boolean) => void;
  bypass: boolean;
  onBypassChange: ((on: boolean) => void) | undefined;
  showMode: boolean;
  agentPicker: ReactNode;
  modelPicker: ReactNode;
  runLocation: ReactNode;
  contextMeter: ReactNode;
}

type ModeValue = 'act' | 'plan' | 'bypass';

const modeValue = (planMode: boolean, bypass: boolean): ModeValue => {
  if (planMode) return 'plan';
  return bypass ? 'bypass' : 'act';
};

/** The footer's left side: Act | Plan (| Bypass, for a local thread), the agent·version picker, the model, where the thread runs, the meters. */
export function FooterControls({ planMode, onPlanModeChange, bypass, onBypassChange, showMode, agentPicker, modelPicker, runLocation, contextMeter }: FooterControlsProps): React.JSX.Element {
  const options: { value: ModeValue; label: string; ariaLabel?: string }[] = [
    { value: 'act', label: t('workspace.mode.act', 'Act') },
    { value: 'plan', label: t('workspace.mode.plan', 'Plan'), ariaLabel: t('workspace.planMode', 'Plan mode (no changes)') },
  ];
  if (onBypassChange !== undefined) options.push({ value: 'bypass', label: t('workspace.mode.bypass', 'Bypass'), ariaLabel: t('workspace.bypassMode', 'Bypass mode (no approvals)') });
  // Bypass turns plan mode off only once it is on (after its warning, the first time): a cancelled warning keeps the mode.
  const change = (next: ModeValue): void => {
    if (next === 'bypass') {
      onBypassChange?.(true);
      return;
    }
    onPlanModeChange(next === 'plan');
    onBypassChange?.(false);
  };
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, minWidth: 0 }}>
      {showMode && (
        <Segmented
          ariaLabel={t('workspace.mode', 'Mode')}
          value={modeValue(planMode, bypass)}
          onChange={change}
          testId="mode-segmented"
          options={options}
        />
      )}
      {agentPicker}
      {modelPicker}
      {runLocation}
      {contextMeter}
    </Box>
  );
}
