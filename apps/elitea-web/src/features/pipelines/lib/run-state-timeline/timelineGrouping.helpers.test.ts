import { describe, expect, it } from 'vitest';

import { PipelineStatus } from '../flow-editor/constants/flowEditor.constants';
import type { GroupedTimelineStep } from './timeline.types';
import {
  findLastOuterStepIndex,
  findPreviousScopedStep,
  formatBadgeCount,
  getActiveRowPosition,
  getVisibleSteps,
  groupConsecutiveSteps,
  isRowActive,
} from './timelineGrouping.helpers';

const step = (
  groupId: string | undefined,
  overrides: Partial<GroupedTimelineStep> = {},
): GroupedTimelineStep => ({
  id: overrides.id ?? groupId ?? 'step',
  created_at: 1_700_000_000_000,
  status: PipelineStatus.Completed,
  ...(groupId !== undefined ? { group_id: groupId } : {}),
  ...overrides,
});

describe('groupConsecutiveSteps', () => {
  it('returns no groups for empty input', () => {
    expect(groupConsecutiveSteps([])).toEqual([]);
  });

  it('merges consecutive steps sharing a non-empty group_id', () => {
    expect(groupConsecutiveSteps([step('a'), step('a'), step('a')])).toEqual([
      {
        groupId: 'a',
        groupLabel: 'a',
        start: 0,
        end: 2,
        stepCount: 3,
        hasContinuedFailure: false,
        hasError: false,
      },
    ]);
  });

  it('starts a new group whenever group_id changes and does not merge non-adjacent groups', () => {
    const groups = groupConsecutiveSteps([step('a'), step('b'), step('a')]);
    expect(groups.map(g => [g.groupId, g.start, g.end, g.stepCount])).toEqual([
      ['a', 0, 0, 1],
      ['b', 1, 1, 1],
      ['a', 2, 2, 1],
    ]);
  });

  it('keeps steps without a group_id separate even when adjacent', () => {
    const groups = groupConsecutiveSteps([step(undefined), step(undefined), step('')]);
    expect(groups.map(g => [g.groupId, g.start, g.end])).toEqual([
      [undefined, 0, 0],
      [undefined, 1, 1],
      ['', 2, 2],
    ]);
  });

  it('propagates group-level continued-failure and error flags', () => {
    const continued = new Map([[1, [{ node: 'worker', handled: 'continue' as const }]]]);
    const groups = groupConsecutiveSteps(
      [step('g1', { id: 'worker', group_label: 'Worker Group' }), step('g1', { id: 'worker', status: PipelineStatus.Error })],
      continued,
    );
    expect(groups[0]).toMatchObject({
      groupLabel: 'Worker Group',
      hasContinuedFailure: true,
      hasError: true,
      stepCount: 2,
    });
  });
});

describe('getVisibleSteps', () => {
  const groups = [
    { start: 0, end: 0 },
    { start: 1, end: 3 },
    { start: 4, end: 5 },
  ];

  it('shows one row per group when collapsed', () => {
    expect(getVisibleSteps(groups, false, new Set())).toEqual([
      { index: 0, end: 0, expanded: false },
      { index: 1, end: 3, expanded: false },
      { index: 4, end: 5, expanded: false },
    ]);
  });

  it('never marks a single-step group expanded even in expand-all mode', () => {
    expect(getVisibleSteps(groups, true, new Set())[0]).toEqual({ index: 0, end: 0, expanded: false });
  });

  it('expands a toggled group into head plus child rows', () => {
    expect(getVisibleSteps(groups, false, new Set([1]))).toEqual([
      { index: 0, end: 0, expanded: false },
      { index: 1, end: 3, expanded: true },
      { index: 2, end: 2, expanded: false },
      { index: 3, end: 3, expanded: false },
      { index: 4, end: 5, expanded: false },
    ]);
  });

  it('allows collapsing one group while expand-all is enabled', () => {
    const rows = getVisibleSteps(groups, true, new Set([1]));
    expect(rows.map(r => r.index)).toEqual([0, 1, 4, 5]);
    expect(rows.find(r => r.index === 1)?.expanded).toBe(false);
    expect(rows.find(r => r.index === 4)?.expanded).toBe(true);
  });
});

describe('getActiveRowPosition & isRowActive & formatBadgeCount', () => {
  const collapsed = [
    { index: 0, end: 0, expanded: false },
    { index: 1, end: 3, expanded: false },
    { index: 4, end: 5, expanded: false },
  ];
  const expanded = [
    { index: 0, end: 0, expanded: false },
    { index: 1, end: 3, expanded: true },
    { index: 2, end: 2, expanded: false },
    { index: 3, end: 3, expanded: false },
    { index: 4, end: 5, expanded: false },
  ];

  it('maps in-progress step to collapsed group or exact expanded child row', () => {
    expect(getActiveRowPosition(collapsed, -1)).toBe(-1);
    expect(getActiveRowPosition(collapsed, 2)).toBe(1);
    expect(getActiveRowPosition(expanded, 2)).toBe(2);
    expect(getActiveRowPosition(expanded, 1)).toBe(1);
  });

  it('marks collapsed group active when a hidden child is selected', () => {
    expect(isRowActive(1, 3, false, 2)).toBe(true);
    expect(isRowActive(1, 3, true, 2)).toBe(false);
    expect(isRowActive(1, 3, true, 1)).toBe(true);
  });

  it('caps badge count at 99+', () => {
    expect(formatBadgeCount(2)).toBe('2');
    expect(formatBadgeCount(99)).toBe('99');
    expect(formatBadgeCount(100)).toBe('99+');
  });
});

describe('findPreviousScopedStep & findLastOuterStepIndex', () => {
  it('compares nested steps within their own scope_id and outer steps against prior outer steps', () => {
    const steps: GroupedTimelineStep[] = [
      step('outer-0', { id: 'start', is_nested: false }),
      step('app:1', { id: 'wrapper', is_nested: false, scope_id: 'app:1' }),
      step('app:1', { id: 'child-a', is_nested: true, scope_id: 'app:1' }),
      step('app:1', { id: 'grandchild', is_nested: true, scope_id: 'app:2' }),
      step('app:1', { id: 'child-b', is_nested: true, scope_id: 'app:1' }),
      step('outer-2', { id: 'finish', is_nested: false }),
    ];

    expect(findPreviousScopedStep(steps, 4)?.id).toBe('child-a');
    expect(findPreviousScopedStep(steps, 3)).toBeUndefined();
    expect(findPreviousScopedStep(steps, 5)?.id).toBe('wrapper');
    expect(findLastOuterStepIndex(steps)).toBe(5);
  });
});
