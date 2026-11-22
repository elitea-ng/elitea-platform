/**
 * Global "Expand all steps" switch for grouped timelines (`EL-3734` / `#1585`,
 * ported from `EliteaUI` `TimeLineExpandAllSwitch.jsx`).
 */
import type { ChangeEvent, ReactNode } from 'react';
import { memo, useCallback } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseSwitch } from '@/shared/ui/BaseSwitch';

import { expandAllContainerSx } from './runStateTimeline.styles';

export interface TimeLineExpandAllSwitchProps {
  readonly checked: boolean;
  readonly onChange: (checked: boolean) => void;
}

export const TimeLineExpandAllSwitch = memo(function TimeLineExpandAllSwitch({
  checked,
  onChange,
}: TimeLineExpandAllSwitchProps): ReactNode {
  const label = t('pipelines.flowEditor.state.expandAllSteps', 'Expand all steps');

  const handleChange = useCallback(
    (_event: ChangeEvent<HTMLInputElement>, nextChecked: boolean) => {
      onChange(nextChecked);
    },
    [onChange],
  );

  return (
    <Box
      component="label"
      sx={expandAllContainerSx}
    >
      <BaseSwitch
        checked={checked}
        onChange={handleChange}
        aria-label={label}
        slotProps={{
          input: {
            role: 'switch',
            'aria-label': label,
            'data-testid': 'pipeline-run-details-timeline-expand-all',
          } as React.InputHTMLAttributes<HTMLInputElement>,
        }}
      />
      <Typography
        variant="bodySmall"
        color="text.secondary"
      >
        {label}
      </Typography>
    </Box>
  );
});
