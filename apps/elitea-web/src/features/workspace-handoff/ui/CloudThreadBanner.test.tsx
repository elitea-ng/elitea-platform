import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { HandoffStatus } from '@/shared/desktop/handoffIpc';
import { LOCAL_STATUS } from '@/shared/desktop/handoffIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { CloudThreadBanner } from './CloudThreadBanner';
import { ExecutorBadge } from './ExecutorBadge';

const CLOUD: HandoffStatus = { ...LOCAL_STATUS, state: 'CLOUD_IDLE', executor: 'cloud', read_only: true, sent_from_here: true, epoch: 2 };

describe('ExecutorBadge', () => {
  it.each([
    ['local', 'Local'],
    ['cloud', 'In cloud'],
    ['returning', 'Returning'],
  ] as const)('says %s as "%s"', (executor, label) => {
    renderWithTheme(<ExecutorBadge executor={executor} />);
    expect(screen.getByTestId('executor-badge')).toHaveTextContent(label);
    expect(screen.getByTestId('executor-badge')).toHaveAttribute('data-executor', executor);
  });
});

describe('CloudThreadBanner', () => {
  it('says the thread runs in the cloud and offers Take back and Erase cloud copy', async () => {
    const user = userEvent.setup();
    const onTakeBack = vi.fn();
    const onErase = vi.fn();
    renderWithTheme(<CloudThreadBanner status={CLOUD} onTakeBack={onTakeBack} onErase={onErase} />);

    const banner = screen.getByTestId('cloud-thread-banner');
    expect(banner).toHaveTextContent('This thread is running in the cloud executor. Local changes are not seen by it.');
    expect(banner).toHaveAttribute('data-executor', 'cloud');
    expect(screen.queryByTestId('banner-reclaim-pending')).toBeNull();

    await user.click(screen.getByRole('button', { name: 'Take back' }));
    await user.click(screen.getByRole('button', { name: 'Erase cloud copy' }));
    expect(onTakeBack).toHaveBeenCalledTimes(1);
    expect(onErase).toHaveBeenCalledTimes(1);
  });

  it('shows a take back that is waiting for the running cloud turn', () => {
    renderWithTheme(<CloudThreadBanner status={{ ...CLOUD, state: 'CLOUD_RUNNING', reclaim_pending: true }} onTakeBack={vi.fn()} onErase={vi.fn()} />);
    expect(screen.getByTestId('banner-reclaim-pending')).toHaveTextContent('starts when the running cloud turn ends');
  });

  it('shows a thread on its way back, and keeps Take back to finish it', () => {
    renderWithTheme(<CloudThreadBanner status={{ ...CLOUD, state: 'RECLAIMING', executor: 'returning' }} onTakeBack={vi.fn()} onErase={vi.fn()} />);
    expect(screen.getByTestId('cloud-thread-banner')).toHaveAttribute('data-executor', 'returning');
    expect(screen.getByTestId('banner-returning')).toHaveTextContent('Finish taking it back');
    expect(screen.getByRole('button', { name: 'Take back' })).toBeEnabled();
  });

  it('cannot take back a thread another computer sent', () => {
    renderWithTheme(<CloudThreadBanner status={{ ...CLOUD, sent_from_here: false, device_name: 'Work laptop' }} onTakeBack={vi.fn()} onErase={vi.fn()} />);
    expect(screen.getByRole('button', { name: 'Take back' })).toBeDisabled();
    expect(screen.getByTestId('banner-other-device')).toHaveTextContent('sent from Work laptop');
    expect(screen.getByRole('button', { name: 'Erase cloud copy' })).toBeEnabled();
  });
});
