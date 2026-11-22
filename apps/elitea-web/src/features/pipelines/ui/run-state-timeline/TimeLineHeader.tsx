/**
 * Header row for the grouped RunStateDialog timeline (`EL-3734` / `#1585`,
 * ported from `EliteaUI` `TimeLineHeader.jsx`).
 */
import type { ReactNode } from 'react';
import { memo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { PipelineStatus } from '../../lib/flow-editor/constants/flowEditor.constants';
import type { GroupedTimelineStep } from '../../lib/run-state-timeline/timeline.types';

import {
  timelineCurrentStepSx,
  timelineHeaderActionsSx,
  timelineHeaderSx,
} from './runStateTimeline.styles';
import { TimeLineExpandAllSwitch } from './TimeLineExpandAllSwitch';
import { TimeLineRunStatus } from './TimeLineRunStatus';

export interface TimeLineHeaderProps {
  readonly steps: readonly GroupedTimelineStep[];
  readonly outerSteps: readonly GroupedTimelineStep[];
  readonly status: string;
  readonly selectedStep: number;
  readonly hasGroups: boolean;
  readonly expandAll: boolean;
  readonly onToggleExpandAll: (checked: boolean) => void;
}

export const TimeLineHeader = memo(function TimeLineHeader({
  steps,
  outerSteps,
  status,
  selectedStep,
  hasGroups,
  expandAll,
  onToggleExpandAll,
}: TimeLineHeaderProps): ReactNode {
  const showRunStatus =
    status === PipelineStatus.InProgress ||
    status === PipelineStatus.Error ||
    status === PipelineStatus.Interrupt;

  const currentStepName =
    status === PipelineStatus.InProgress
      ? (outerSteps.at(-2)?.id ?? t('pipelines.flowEditor.state.start', 'Start'))
      : steps[selectedStep]?.id;

  return (
    <Box
      sx={timelineHeaderSx}
      data-testid="pipeline-run-details-timeline-section"
    >
      <Box sx={timelineCurrentStepSx}>
        <Typography
          variant="subtitle"
          color="text.primary"
        >
          {t('pipelines.flowEditor.state.timelineStep', 'Timeline step:')}
        </Typography>
        <Typography
          variant="bodyMedium"
          color="text.secondary"
        >
          {currentStepName}
        </Typography>
      </Box>
      <Box sx={timelineHeaderActionsSx}>
        {hasGroups && (
          <TimeLineExpandAllSwitch
            checked={expandAll}
            onChange={onToggleExpandAll}
          />
        )}
        {showRunStatus && <TimeLineRunStatus lastStep={outerSteps.at(-1)} />}
      </Box>
    </Box>
  );
});
