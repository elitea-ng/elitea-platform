/**
 * Pure timeline grouping, visibility, active-row, badge-count, and scoped-diff
 * helpers ported from `EliteaUI` `EL-3734` (`#1585`, `timeLine.helpers.js`).
 */
import { PipelineStatus } from '../flow-editor/constants/flowEditor.constants';

import { MAX_BADGE_COUNT } from './timeline.constants';
import type {
  GroupedTimelineStep,
  TimelineContinuedFailure,
  TimelineStepGroup,
  VisibleTimelineRow,
} from './timeline.types';

/**
 * Splits steps into runs of consecutive steps that share a non-empty `group_id`.
 * A step without a `group_id` always forms a group of its own.
 */
export function groupConsecutiveSteps(
  steps: readonly GroupedTimelineStep[],
  continuedByStep?: ReadonlyMap<number, readonly TimelineContinuedFailure[]>,
): readonly TimelineStepGroup[] {
  const groups: Array<{
    groupId: string | undefined;
    groupLabel: string;
    start: number;
    end: number;
    hasContinuedFailure: boolean;
    hasError: boolean;
  }> = [];

  steps.forEach((step, index) => {
    const stepHasContinued = (continuedByStep?.get(index)?.length ?? 0) > 0;
    const stepHasError = step.status === PipelineStatus.Error;
    const previous = groups.at(-1);

    if (step.group_id && previous?.groupId === step.group_id) {
      previous.end = index;
      previous.hasContinuedFailure = previous.hasContinuedFailure || stepHasContinued;
      previous.hasError = previous.hasError || stepHasError;
      return;
    }

    groups.push({
      groupId: step.group_id,
      groupLabel: step.group_label || step.id,
      start: index,
      end: index,
      hasContinuedFailure: stepHasContinued,
      hasError: stepHasError,
    });
  });

  return groups.map(group => ({
    ...group,
    stepCount: group.end - group.start + 1,
  }));
}

/**
 * Lists the rows to draw. A group is expanded when the "expand all" mode
 * differs from its own override, so groups that arrive during a live run
 * follow the mode without being tracked individually.
 */
export function getVisibleSteps(
  groups: readonly Pick<TimelineStepGroup, 'start' | 'end'>[],
  expandAll: boolean,
  toggledGroups: ReadonlySet<number>,
): readonly VisibleTimelineRow[] {
  return groups.flatMap(({ start, end }) => {
    const expanded = end > start && expandAll !== toggledGroups.has(start);
    const rows: VisibleTimelineRow[] = [{ index: start, end, expanded }];
    if (expanded) {
      for (let index = start + 1; index <= end; index += 1) {
        rows.push({ index, end: index, expanded: false });
      }
    }
    return rows;
  });
}

/** Position of the row that holds the running step, or -1 when nothing is running. */
export function getActiveRowPosition(visibleSteps: readonly VisibleTimelineRow[], inProgressStep: number): number {
  if (inProgressStep < 0) return -1;
  return visibleSteps.findIndex(({ index, end, expanded }) =>
    expanded ? index === inProgressStep : index <= inProgressStep && inProgressStep <= end,
  );
}

/** Text for a count badge. Counts above the cap collapse to e.g. `99+`; the exact value belongs in a label. */
export function formatBadgeCount(count: number): string {
  return count > MAX_BADGE_COUNT ? `${MAX_BADGE_COUNT}+` : String(count);
}

/** A row is active when it is selected, or when it is a collapsed group that holds the selected step. */
export function isRowActive(index: number, end: number, expanded: boolean, selectedStep: number): boolean {
  return index === selectedStep || (!expanded && index < selectedStep && selectedStep <= end);
}

/**
 * Finds the preceding step in the same execution scope for before/after state
 * comparison (`EL-3734` `RunStateDialog.jsx` L30-35): child state belongs to its
 * immediate parent's invocation (`scope_id`), not the enclosing pipeline.
 */
export function findPreviousScopedStep(
  steps: readonly GroupedTimelineStep[],
  selectedStep: number,
): GroupedTimelineStep | undefined {
  const currentStep = steps[selectedStep];
  if (!currentStep) return undefined;

  for (let index = selectedStep - 1; index >= 0; index -= 1) {
    const candidate = steps[index];
    if (!candidate) continue;
    if (currentStep.is_nested) {
      if (candidate.is_nested && candidate.scope_id === currentStep.scope_id) return candidate;
    } else if (!candidate.is_nested) {
      return candidate;
    }
  }
  return undefined;
}

/** Index of the last outer (non-nested) step, or `-1` when none exist. */
export function findLastOuterStepIndex(steps: readonly GroupedTimelineStep[]): number {
  for (let index = steps.length - 1; index >= 0; index -= 1) {
    if (!steps[index]?.is_nested) return index;
  }
  return -1;
}
