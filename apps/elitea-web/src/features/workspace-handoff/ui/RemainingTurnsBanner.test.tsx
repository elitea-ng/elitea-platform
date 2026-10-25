import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { HandoffStatus } from '@/shared/desktop/handoffIpc';
import { LOCAL_STATUS } from '@/shared/desktop/handoffIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { RemainingTurnsBanner, remainingTurns } from './RemainingTurnsBanner';

const KEPT: HandoffStatus = { ...LOCAL_STATUS, epoch: 5, sent_from_here: true, rest_pending: true, rest_turns: 2, delete_after: '2026-10-22T00:00:00Z' };

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
});
