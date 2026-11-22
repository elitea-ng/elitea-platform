import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { GroupedTimelineStep } from './timeline.types';
import { useTimeLineGroups } from './useTimeLineGroups';

const makeStep = (id: string, groupId?: string): GroupedTimelineStep => ({
  id,
  created_at: 1_700_000_000_000,
  ...(groupId !== undefined ? { group_id: groupId } : {}),
});

const steps: GroupedTimelineStep[] = [
  makeStep('start', 'a'),
  makeStep('agent', 'b'),
  makeStep('tool-1', 'b'),
  makeStep('tool-2', 'b'),
  makeStep('end', 'c'),
];

describe('useTimeLineGroups', () => {
  it('starts collapsed and detects multi-step groups', () => {
    const { result } = renderHook(() => useTimeLineGroups(steps, 0, vi.fn()));

    expect(result.current.expandAll).toBe(false);
    expect(result.current.hasGroups).toBe(true);
    expect(result.current.visibleSteps.map(r => r.index)).toEqual([0, 1, 4]);
  });

  it('reports hasGroups=false when every step is standalone', () => {
    const standalone = [makeStep('s1', 'a'), makeStep('s2', 'b'), makeStep('s3')];
    const { result } = renderHook(() => useTimeLineGroups(standalone, 0, vi.fn()));
    expect(result.current.hasGroups).toBe(false);
  });

  it('expands and collapses an individual group and hands selection to head on collapse', () => {
    const onSelect = vi.fn();
    const { result } = renderHook(() => useTimeLineGroups(steps, 2, onSelect));

    act(() => result.current.toggleGroup(1, 3, false));
    expect(result.current.visibleSteps.map(r => r.index)).toEqual([0, 1, 2, 3, 4]);
    expect(onSelect).not.toHaveBeenCalled();

    act(() => result.current.toggleGroup(1, 3, true));
    expect(result.current.visibleSteps.map(r => r.index)).toEqual([0, 1, 4]);
    expect(onSelect).toHaveBeenCalledWith(1);
  });

  it('applies expandAll to groups arriving later and resets per-group overrides when toggled', () => {
    const onSelect = vi.fn();
    const { result, rerender } = renderHook(
      ({ list, selected }) => useTimeLineGroups(list, selected, onSelect),
      { initialProps: { list: steps, selected: 3 } },
    );

    act(() => result.current.toggleExpandAll(true));
    expect(result.current.visibleSteps.map(r => r.index)).toEqual([0, 1, 2, 3, 4]);

    rerender({ list: [...steps, makeStep('extra-1', 'd'), makeStep('extra-2', 'd')], selected: 3 });
    expect(result.current.visibleSteps.map(r => r.index)).toEqual([0, 1, 2, 3, 4, 5, 6]);

    act(() => result.current.toggleExpandAll(false));
    expect(result.current.visibleSteps.map(r => r.index)).toEqual([0, 1, 4, 5]);
    expect(onSelect).toHaveBeenCalledWith(1);
  });

  it('keeps handler identity stable when selectedStep changes', () => {
    const onSelect = vi.fn();
    const { result, rerender } = renderHook(
      ({ selected }) => useTimeLineGroups(steps, selected, onSelect),
      { initialProps: { selected: 0 } },
    );
    const { toggleGroup, toggleExpandAll } = result.current;

    rerender({ selected: 2 });
    expect(result.current.toggleGroup).toBe(toggleGroup);
    expect(result.current.toggleExpandAll).toBe(toggleExpandAll);

    act(() => result.current.toggleGroup(1, 3, true));
    expect(onSelect).toHaveBeenCalledWith(1);
  });
});
