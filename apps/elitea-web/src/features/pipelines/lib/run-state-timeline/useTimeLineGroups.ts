/**
 * React hook managing grouped timeline expand/collapse state and selection
 * hand-off (`EL-3734` / `#1585`, ported from `useTimeLineGroups.hooks.js`).
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import type {
  GroupedTimelineStep,
  TimelineContinuedFailure,
  TimelineGroupState,
} from './timeline.types';
import { getVisibleSteps, groupConsecutiveSteps } from './timelineGrouping.helpers';

export function useTimeLineGroups(
  steps: readonly GroupedTimelineStep[],
  selectedStep: number,
  onSelect: (index: number) => void,
  continuedByStep?: ReadonlyMap<number, readonly TimelineContinuedFailure[]>,
): TimelineGroupState {
  const [expandAll, setExpandAll] = useState(false);
  const [toggledGroups, setToggledGroups] = useState<ReadonlySet<number>>(() => new Set());
  // Read through a ref so the handlers keep their identity while the selection follows a running step.
  const selectedStepRef = useRef(selectedStep);

  useEffect(() => {
    selectedStepRef.current = selectedStep;
  }, [selectedStep]);

  const groups = useMemo(
    () => groupConsecutiveSteps(steps, continuedByStep),
    [steps, continuedByStep],
  );
  const visibleSteps = useMemo(
    () => getVisibleSteps(groups, expandAll, toggledGroups),
    [groups, expandAll, toggledGroups],
  );
  const hasGroups = useMemo(() => groups.some(({ start, end }) => end > start), [groups]);

  const toggleGroup = useCallback(
    (start: number, end: number, expanded: boolean) => {
      setToggledGroups(previous => {
        const next = new Set(previous);
        if (next.has(start)) next.delete(start);
        else next.add(start);
        return next;
      });
      // Collapsing hides the selected child, so the group head takes over the selection.
      if (expanded && selectedStepRef.current > start && selectedStepRef.current <= end) {
        onSelect(start);
      }
    },
    [onSelect],
  );

  const toggleExpandAll = useCallback(
    (checked: boolean) => {
      setExpandAll(checked);
      setToggledGroups(new Set());
      if (!checked) {
        const selectedGroup = groups.find(
          ({ start, end }) => selectedStepRef.current > start && selectedStepRef.current <= end,
        );
        if (selectedGroup) onSelect(selectedGroup.start);
      }
    },
    [groups, onSelect],
  );

  return { expandAll, hasGroups, groups, visibleSteps, toggleGroup, toggleExpandAll };
}
