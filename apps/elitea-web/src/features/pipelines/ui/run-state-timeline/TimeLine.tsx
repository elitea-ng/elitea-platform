/**
 * Grouped RunStateDialog timeline (`EL-3734` / `#1585`, ported from `EliteaUI`
 * `TimeLine.jsx` + replatform 5b-1 D12 `node_errors` `"Failed · continued"`
 * attribution and structured support-ref display).
 */
import type { ReactNode } from 'react';
import { memo, useMemo } from 'react';

import Box from '@mui/material/Box';
import StepConnector from '@mui/material/StepConnector';
import Stepper from '@mui/material/Stepper';

import { t } from '@/shared/i18n';

import { PipelineStatus } from '../../lib/flow-editor/constants/flowEditor.constants';
import type { GroupedTimelineStep } from '../../lib/run-state-timeline/timeline.types';
import {
  getActiveRowPosition,
  isRowActive,
} from '../../lib/run-state-timeline/timelineGrouping.helpers';
import {
  collectContinuedFailuresInRange,
  extractContinuedFailuresByStep,
} from '../../lib/run-state-timeline/timelineNodeErrors.helpers';
import { useTimeLineGroups } from '../../lib/run-state-timeline/useTimeLineGroups';

import {
  timelineConnectorSx,
  timelineScrollSx,
  timelineStepperSx,
} from './runStateTimeline.styles';
import { TimeLineContinuedFailureSummary } from './TimeLineContinuedFailureSummary';
import { TimeLineHeader } from './TimeLineHeader';
import { TimeLineStep } from './TimeLineStep';

export interface TimeLineProps {
  readonly steps: readonly GroupedTimelineStep[];
  readonly outerSteps?: readonly GroupedTimelineStep[] | undefined;
  readonly status: string;
  readonly selectedStep: number;
  readonly onSelect: (index: number) => void;
}

export const TimeLine = memo(function TimeLine({
  steps,
  outerSteps,
  status,
  selectedStep,
  onSelect,
}: TimeLineProps): ReactNode {
  const effectiveOuterSteps = useMemo(
    () => outerSteps ?? steps.filter(step => !step.is_nested),
    [outerSteps, steps],
  );
  const continuedByStep = useMemo(() => extractContinuedFailuresByStep(steps), [steps]);

  const { expandAll, hasGroups, visibleSteps, toggleGroup, toggleExpandAll } = useTimeLineGroups(
    steps,
    selectedStep,
    onSelect,
    continuedByStep,
  );

  const isError = status === PipelineStatus.Error;
  const inProgressStep = steps.findIndex(step => step.status === PipelineStatus.InProgress);

  const selectedRow = visibleSteps.find(({ index, end, expanded }) =>
    isRowActive(index, end, expanded, selectedStep),
  );
  const selectedContinuedFailures = useMemo(() => {
    if (!selectedRow) return [];
    const rangeEnd = selectedRow.expanded ? selectedRow.index : selectedRow.end;
    return collectContinuedFailuresInRange(continuedByStep, selectedRow.index, rangeEnd);
  }, [continuedByStep, selectedRow]);

  return (
    <>
      <TimeLineHeader
        steps={steps}
        outerSteps={effectiveOuterSteps}
        status={status}
        selectedStep={selectedStep}
        hasGroups={hasGroups}
        expandAll={expandAll}
        onToggleExpandAll={toggleExpandAll}
      />
      <Box
        component="section"
        sx={timelineScrollSx}
        aria-label={t('pipelines.flowEditor.state.timelineRegionLabel', 'Pipeline timeline steps')}
        tabIndex={0}
      >
        <Stepper
          sx={timelineStepperSx}
          activeStep={getActiveRowPosition(visibleSteps, inProgressStep)}
          connector={<StepConnector sx={timelineConnectorSx(isError)} />}
        >
          {visibleSteps.map(({ index, end, expanded }) => {
            const step = steps[index];
            if (!step) return null;
            const rowContinuedFailures = collectContinuedFailuresInRange(
              continuedByStep,
              index,
              expanded ? index : end,
            );
            return (
              <TimeLineStep
                key={index}
                stepIndex={index}
                end={end}
                expanded={expanded}
                active={isRowActive(index, end, expanded, selectedStep)}
                step={step}
                isError={isError || step.status === PipelineStatus.Error}
                continuedFailures={rowContinuedFailures}
                onSelect={onSelect}
                onToggleGroup={toggleGroup}
              />
            );
          })}
        </Stepper>
      </Box>
      <TimeLineContinuedFailureSummary failures={selectedContinuedFailures} />
    </>
  );
});
