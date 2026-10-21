import { act, renderHook } from '@testing-library/react-native';

import { activityGlyphOf, RECENT_FINISH_MS } from '@/features/chats/activity';
import { COARSE_CLOCK_MS, useCoarseClock } from '@/lib/useCoarseClock';

beforeEach(() => jest.useFakeTimers({ now: Date.parse('2026-10-21T12:00:00Z') }));
afterEach(() => jest.useRealTimers());

describe('useCoarseClock', () => {
  it('moves on every interval while active, so a mounted list recomputes what expires (a finished turn check)', async () => {
    // A turn that finished just short of a day ago: its check is still drawn.
    const row = { last_activity_at: new Date(Date.now() - RECENT_FINISH_MS + 30_000).toISOString(), unread: true };
    const { result } = await renderHook(() => activityGlyphOf(row, useCoarseClock()));
    expect(result.current).toBe('unread');
    await act(async () => void jest.advanceTimersByTime(COARSE_CLOCK_MS));
    expect(result.current).toBeNull();
  });

  it('stands still while inactive and catches up at once when active again', async () => {
    const { result, rerender } = await renderHook((props: { active: boolean }) => useCoarseClock(props.active), { initialProps: { active: false } });
    const start = result.current;
    await act(async () => void jest.advanceTimersByTime(COARSE_CLOCK_MS * 3));
    expect(result.current).toBe(start);
    await rerender({ active: true });
    await act(async () => void jest.advanceTimersByTime(0));
    expect(result.current).toBe(start + COARSE_CLOCK_MS * 3);
  });
});
