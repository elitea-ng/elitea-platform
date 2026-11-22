/**
 * Step name, collapsed step-count badge, and group toggle button rendered
 * above each timeline dot (`EL-3734` / `#1585`, ported from `EliteaUI`
 * `TimeLineStepName.jsx`).
 */
import type { ReactNode } from 'react';
import { memo } from 'react';

import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';
import { EllipsisTypography } from '@/shared/ui/EllipsisTypography';

import { MAX_BADGE_COUNT } from '../../lib/run-state-timeline/timeline.constants';
import { formatBadgeCount } from '../../lib/run-state-timeline/timelineGrouping.helpers';

import {
  stepCountBadgeSx,
  stepCountBadgeWrapperSx,
  stepNameContainerSx,
  stepNameTextSx,
  stepNameWithCountSx,
} from './runStateTimeline.styles';
import { TimeLineGroupToggle } from './TimeLineGroupToggle';

export interface TimeLineStepNameProps {
  readonly index: number;
  readonly name: string;
  readonly groupLabel: string;
  readonly stepCount?: number | undefined;
  readonly hasGroup: boolean;
  readonly expanded: boolean;
  readonly toggleLabel: string;
  readonly onToggle: () => void;
}

export const TimeLineStepName = memo(function TimeLineStepName({
  index,
  name,
  groupLabel,
  stepCount,
  hasGroup,
  expanded,
  toggleLabel,
  onToggle,
}: TimeLineStepNameProps): ReactNode {
  const badgeTooltip =
    stepCount !== undefined && stepCount > MAX_BADGE_COUNT
      ? t('pipelines.flowEditor.state.timelineGroupStepCount', '{{count}} steps', { count: stepCount })
      : '';

  return (
    <Box sx={stepNameContainerSx}>
      <Box sx={stepNameWithCountSx}>
        <EllipsisTypography sx={stepNameTextSx}>{name}</EllipsisTypography>
        {stepCount !== undefined && (
          <Tooltip title={badgeTooltip}>
            <Box sx={stepCountBadgeWrapperSx}>
              <Box
                component="span"
                sx={stepCountBadgeSx}
                aria-label={t(
                  'pipelines.flowEditor.state.timelineGroupStepCountInGroup',
                  '{{count}} steps in {{groupLabel}}',
                  { count: stepCount, groupLabel },
                )}
                data-testid={`pipeline-run-details-timeline-group-count-${index}`}
              >
                {formatBadgeCount(stepCount)}
              </Box>
            </Box>
          </Tooltip>
        )}
      </Box>
      {hasGroup && (
        <TimeLineGroupToggle
          label={toggleLabel}
          expanded={expanded}
          onToggle={onToggle}
          testId={`pipeline-run-details-timeline-group-toggle-${index}`}
        />
      )}
    </Box>
  );
});
