/**
 * Single timeline step node (name + group toggle + accessible dot button +
 * formatted timestamp) for the grouped RunStateDialog timeline (`EL-3734` /
 * `#1585`, ported from `EliteaUI` `TimeLineStep.jsx`, `TimeLineStepLabel.jsx`,
 * and `ProcessStepIcon.jsx` + replatform `"Failed · continued"` status).
 */
import type { ReactNode } from 'react';
import { memo, useCallback } from 'react';

import { format } from 'date-fns';

import Box from '@mui/material/Box';
import Step from '@mui/material/Step';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type {
  GroupedTimelineStep,
  TimelineContinuedFailure,
} from '../../lib/run-state-timeline/timeline.types';

import {
  stepDotInnerSx,
  stepDotOuterSx,
  stepTimestampContainerSx,
  stepTimestampRowSx,
  timelineStepSx,
} from './runStateTimeline.styles';
import { TimeLineStepName } from './TimeLineStepName';

export interface TimeLineStepProps {
  /** Injected by MUI `Stepper` (visual row position). */
  readonly index?: number | undefined;
  /** Injected by MUI `Stepper`. */
  readonly last?: boolean | undefined;
  readonly stepIndex: number;
  readonly end: number;
  readonly expanded: boolean;
  readonly active: boolean;
  readonly step: GroupedTimelineStep;
  readonly isError: boolean;
  readonly continuedFailures?: readonly TimelineContinuedFailure[] | undefined;
  readonly onSelect: (index: number) => void;
  readonly onToggleGroup: (start: number, end: number, expanded: boolean) => void;
}

function formatStepTimestamp(createdAt: number | string): string {
  const date = new Date(createdAt);
  if (Number.isNaN(date.getTime())) return '';
  return format(date, 'HH:mm:ss');
}

function buildStepTooltip(
  stepLabel: string,
  continuedFailures: readonly TimelineContinuedFailure[] | undefined,
): string {
  if (!continuedFailures || continuedFailures.length === 0) return stepLabel;
  const statusText = t('pipelines.flowEditor.state.failedContinued', 'Failed · continued');
  const refs = continuedFailures
    .map(entry => entry.supportRef)
    .filter((ref): ref is string => Boolean(ref));
  if (refs.length === 0) return `${stepLabel} · ${statusText}`;
  return `${stepLabel} · ${statusText} (${refs.join(', ')})`;
}

function resolveToggleLabel(expanded: boolean, groupLabel: string, stepCount: number): string {
  if (expanded) {
    return t('pipelines.flowEditor.state.collapseGroup', 'Collapse {{groupLabel}} ({{count}} steps)', {
      groupLabel,
      count: stepCount,
    });
  }
  return t('pipelines.flowEditor.state.expandGroup', 'Expand {{groupLabel}} ({{count}} steps)', {
    groupLabel,
    count: stepCount,
  });
}

function resolveStepStatus(hasContinuedFailure: boolean, isError: boolean): string {
  if (hasContinuedFailure) return 'failed-continued';
  return isError ? 'error' : 'completed';
}

export const TimeLineStep = memo(function TimeLineStep({
  index: position,
  last,
  stepIndex,
  end,
  expanded,
  active,
  step,
  isError,
  continuedFailures,
  onSelect,
  onToggleGroup,
}: TimeLineStepProps): ReactNode {
  const hasGroup = end > stepIndex;
  const isCollapsedGroup = hasGroup && !expanded;
  const groupLabel = step.group_label || step.id;
  const stepCount = end - stepIndex + 1;
  const hasContinuedFailure = (continuedFailures?.length ?? 0) > 0;
  const dotIsError = isError || hasContinuedFailure;
  const displayName = isCollapsedGroup ? groupLabel : step.id;
  const tooltipText = buildStepTooltip(displayName, continuedFailures);
  const toggleLabel = resolveToggleLabel(expanded, groupLabel, stepCount);
  const dataStatus = resolveStepStatus(hasContinuedFailure, isError);

  const handleToggle = useCallback(() => {
    onToggleGroup(stepIndex, end, expanded);
  }, [end, expanded, stepIndex, onToggleGroup]);

  const handleSelect = useCallback(() => {
    onSelect(stepIndex);
  }, [onSelect, stepIndex]);

  const stepSlotProps = {
    ...(position !== undefined ? { index: position } : {}),
    ...(last !== undefined ? { last } : {}),
  };

  return (
    <Step
      {...stepSlotProps}
      sx={timelineStepSx}
    >
      <TimeLineStepName
        index={stepIndex}
        name={displayName}
        groupLabel={groupLabel}
        stepCount={isCollapsedGroup ? stepCount : undefined}
        hasGroup={hasGroup}
        expanded={expanded}
        toggleLabel={toggleLabel}
        onToggle={handleToggle}
      />
      <Tooltip
        title={tooltipText}
        placement="top"
      >
        <Box
          component="button"
          type="button"
          aria-label={tooltipText}
          aria-pressed={active}
          sx={stepDotOuterSx(active, dotIsError)}
          onClick={handleSelect}
          data-testid={`pipeline-run-details-timeline-step-${stepIndex}`}
          data-status={dataStatus}
        >
          <Box sx={stepDotInnerSx(dotIsError)} />
        </Box>
      </Tooltip>
      <Box sx={stepTimestampContainerSx}>
        <Box sx={stepTimestampRowSx}>
          <Typography
            variant="bodySmall"
            data-testid={`pipeline-run-details-timeline-timestamp-${stepIndex}`}
          >
            {formatStepTimestamp(step.created_at)}
          </Typography>
        </Box>
      </Box>
    </Step>
  );
});
