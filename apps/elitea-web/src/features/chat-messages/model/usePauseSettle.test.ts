import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { PAUSE_SETTLE_MAX_MS, PAUSE_SETTLE_QUIET_MS, isPauseFrameType, usePauseSettle } from './usePauseSettle';

describe('usePauseSettle', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('settles once after the quiet window, extended by each further pause frame', () => {
    const settle = vi.fn();
    const { result } = renderHook(() => usePauseSettle());
    act(() => { result.current.defer(settle); });
    act(() => { vi.advanceTimersByTime(PAUSE_SETTLE_QUIET_MS - 1); });
    act(() => { result.current.defer(settle); });
    act(() => { vi.advanceTimersByTime(PAUSE_SETTLE_QUIET_MS - 1); });
    expect(settle).not.toHaveBeenCalled();
    act(() => { vi.advanceTimersByTime(1); });
    expect(settle).toHaveBeenCalledTimes(1);
  });

  it('never waits longer than the cap however many frames follow', () => {
    const settle = vi.fn();
    const { result } = renderHook(() => usePauseSettle());
    for (let elapsed = 0; elapsed < PAUSE_SETTLE_MAX_MS + PAUSE_SETTLE_QUIET_MS; elapsed += PAUSE_SETTLE_QUIET_MS / 2) {
      act(() => { result.current.defer(settle); });
      act(() => { vi.advanceTimersByTime(PAUSE_SETTLE_QUIET_MS / 2); });
    }
    expect(settle).toHaveBeenCalledTimes(1);
  });

  it('drops an armed window on cancel and on unmount', () => {
    const settle = vi.fn();
    const { result, unmount } = renderHook(() => usePauseSettle());
    act(() => { result.current.defer(settle); });
    act(() => { result.current.cancel(); });
    act(() => { result.current.defer(settle); });
    unmount();
    act(() => { vi.advanceTimersByTime(PAUSE_SETTLE_MAX_MS); });
    expect(settle).not.toHaveBeenCalled();
  });

  it('knows the pause frame types', () => {
    expect([isPauseFrameType('agent_hitl_interrupt'), isPauseFrameType('mcp_authorization_required'), isPauseFrameType('agent_response')]).toEqual([true, true, false]);
  });
});
