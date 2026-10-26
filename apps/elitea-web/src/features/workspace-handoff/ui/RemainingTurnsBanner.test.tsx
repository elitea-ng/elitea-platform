import { act, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { HandoffStatus } from '@/shared/desktop/handoffIpc';
import { LOCAL_STATUS } from '@/shared/desktop/handoffIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { RemainingTurnsBanner, remainingTurns } from './RemainingTurnsBanner';

const KEPT: HandoffStatus = { ...LOCAL_STATUS, epoch: 5, sent_from_here: true, rest_pending: true, rest_turns: 2, delete_after: new Date(Date.now() + 86_400_000).toISOString() };

describe('RemainingTurnsBanner', () => {
  it('offers to take back the later cloud turns while the cloud copy keeps them', async () => {
    const user = userEvent.setup();
    const onTakeBack = vi.fn();
    renderWithTheme(<RemainingTurnsBanner status={KEPT} onTakeBack={onTakeBack} />);

    expect(screen.getByTestId('remaining-turns-banner')).toHaveTextContent('2 later cloud changes were not taken back.');
    expect(screen.getByTestId('remaining-turns-until')).toHaveTextContent('You can take them back until');
    await user.click(screen.getByRole('button', { name: 'Take back the remaining turns' }));
    expect(onTakeBack).toHaveBeenCalledTimes(1);
  });

  it('shows nothing once the grace is over, away from this computer, or in the cloud', () => {
    expect(remainingTurns({ ...KEPT, rest_turns: 0 })).toBe(0);
    expect(remainingTurns({ ...KEPT, sent_from_here: false })).toBe(0);
    expect(remainingTurns({ ...KEPT, executor: 'cloud' })).toBe(0);
    expect(remainingTurns(undefined)).toBe(0);
    expect(remainingTurns(KEPT)).toBe(2);
    const { container } = renderWithTheme(<RemainingTurnsBanner status={{ ...KEPT, rest_turns: 0 }} onTakeBack={vi.fn()} />);
    expect(container).toBeEmptyDOMElement();
  });

  it('hides itself when the grace ends, though the status still counts the turns', () => {
    vi.useFakeTimers();
    try {
      const status = { ...KEPT, delete_after: new Date(Date.now() + 60_000).toISOString() };
      renderWithTheme(<RemainingTurnsBanner status={status} onTakeBack={vi.fn()} />);
      expect(screen.getByTestId('remaining-turns-banner')).toBeInTheDocument();
      act(() => {
        vi.advanceTimersByTime(61_000);
      });
      expect(screen.queryByTestId('remaining-turns-banner')).not.toBeInTheDocument();
      const { container } = renderWithTheme(<RemainingTurnsBanner status={{ ...KEPT, delete_after: '2020-01-01T00:00:00Z' }} onTakeBack={vi.fn()} />);
      expect(container).toBeEmptyDOMElement();
    } finally {
      vi.useRealTimers();
    }
  });
});
