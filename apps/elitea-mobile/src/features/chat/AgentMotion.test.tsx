import { act, render, renderHook, screen } from '@testing-library/react-native';

import { ONE_SHOT_MS, avatarMotion } from '@elitea/client-core/conversation';
import { AccessibilityInfo, Text } from 'react-native';

import { DesignProvider } from '@/design/DesignProvider';
import { AgentMotion, useAvatarClock } from '@/features/chat/AgentMotion';

/** ADR-0050 decision 6: the default motion pack over an agent's avatar. */
describe('AgentMotion', () => {
  afterEach(() => jest.restoreAllMocks());

  it('draws the avatar alone for the still', async () => {
    await render(
      <AgentMotion motion="still" size={28}>
        <Text>M</Text>
      </AgentMotion>,
    );
    expect(screen.getByText('M', { includeHiddenElements: true })).toBeTruthy();
    expect(screen.queryByTestId('agent-motion-still', { includeHiddenElements: true })).toBeNull();
  });

  it.each(['thinking', 'speaking', 'working', 'waiting', 'success', 'error', 'waking'] as const)('draws the %s motion over the avatar', async (motion) => {
    await render(
      <AgentMotion motion={motion} size={28}>
        <Text>M</Text>
      </AgentMotion>,
    );
    expect(screen.getByTestId(`agent-motion-${motion}`, { includeHiddenElements: true })).toBeTruthy();
  });

  it('shows the still under Reduce Motion', async () => {
    jest.spyOn(AccessibilityInfo, 'isReduceMotionEnabled').mockResolvedValue(true);
    await render(
      <DesignProvider>
        <AgentMotion motion="speaking" size={28}>
          <Text>M</Text>
        </AgentMotion>
      </DesignProvider>,
    );
    expect(await screen.findByText('M', { includeHiddenElements: true })).toBeTruthy();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByTestId('agent-motion-speaking', { includeHiddenElements: true })).toBeNull();
  });
});

describe('useAvatarClock', () => {
  afterEach(() => jest.useRealTimers());

  it('is the newest state change, then moves on when a success slot ends', async () => {
    jest.useFakeTimers();
    const states = { 4: { state: 'success' as const, since: 1_000 } };
    const { result } = await renderHook(() => useAvatarClock(states));
    expect(result.current).toBe(1_000);
    expect(avatarMotion({ participantId: 4, newest: true }, states, result.current, { reducedMotion: false })).toBe('success');
    await act(async () => {
      jest.advanceTimersByTime(ONE_SHOT_MS);
    });
    expect(result.current).toBe(1_000 + ONE_SHOT_MS);
    expect(avatarMotion({ participantId: 4, newest: true }, states, result.current, { reducedMotion: false })).toBe('still');
  });
});
