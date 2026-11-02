import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { paneFolds, usePaneFold } from './useEditorPaneRoom';

describe('paneFolds', () => {
  it('folds nothing before the editor has been measured', () => {
    expect(paneFolds(0)).toEqual({ foldChat: false, foldGeneral: false });
  });

  it('keeps both panes open on a wide editor', () => {
    expect(paneFolds(1400)).toEqual({ foldChat: false, foldGeneral: false });
  });

  it('folds the chat pane at a 1280px window with the sidebar open (984px editor)', () => {
    expect(paneFolds(984)).toEqual({ foldChat: true, foldGeneral: false });
  });

  it('folds both panes when even a folded chat leaves the canvas under 480px', () => {
    expect(paneFolds(820)).toEqual({ foldChat: true, foldGeneral: true });
  });
});

describe('usePaneFold', () => {
  it('follows the room until the person toggles, and reports each change', () => {
    const onCollapsed = vi.fn();
    const { result, rerender } = renderHook(({ auto }) => usePaneFold(auto, false, onCollapsed), { initialProps: { auto: false } });
    expect(result.current.collapsed).toBe(false);
    expect(onCollapsed).not.toHaveBeenCalled();

    rerender({ auto: true });
    expect(result.current.collapsed).toBe(true);
    expect(onCollapsed).toHaveBeenLastCalledWith(true);

    act(() => result.current.toggle());
    expect(result.current.collapsed).toBe(false);
    expect(onCollapsed).toHaveBeenLastCalledWith(false);

    // The person's choice outlasts a later verdict of the room.
    rerender({ auto: false });
    rerender({ auto: true });
    expect(result.current.collapsed).toBe(false);
  });

  it('never folds in the stacked small-window layout', () => {
    const { result } = renderHook(() => usePaneFold(true, true, undefined));
    expect(result.current.collapsed).toBe(false);
  });
});
