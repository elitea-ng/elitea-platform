/**
 * Expand/collapse icon button for a grouped timeline step (`EL-3734` / `#1585`,
 * ported from `EliteaUI` `TimeLineGroupToggle.jsx`).
 */
import type { ReactNode } from 'react';
import { memo } from 'react';

import Tooltip from '@mui/material/Tooltip';

import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';
import { CollapseIcon } from '@/shared/ui/icons/collapse-icon';
import { ExpandIcon } from '@/shared/ui/icons/expand-icon';

import { groupToggleBtnSx } from './runStateTimeline.styles';

export interface TimeLineGroupToggleProps {
  readonly label: string;
  readonly expanded: boolean;
  readonly onToggle: () => void;
  readonly testId?: string | undefined;
}

export const TimeLineGroupToggle = memo(function TimeLineGroupToggle({
  label,
  expanded,
  onToggle,
  testId,
}: TimeLineGroupToggleProps): ReactNode {
  return (
    <Tooltip title={label}>
      <BaseBtn
        variant={BUTTON_VARIANTS.icon}
        sx={groupToggleBtnSx}
        aria-label={label}
        aria-expanded={expanded}
        onClick={onToggle}
        {...(testId !== undefined ? { 'data-testid': testId } : {})}
        startIcon={expanded ? <CollapseIcon aria-hidden="true" /> : <ExpandIcon aria-hidden="true" />}
      />
    </Tooltip>
  );
});
