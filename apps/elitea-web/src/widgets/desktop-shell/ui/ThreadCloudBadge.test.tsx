import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { HandoffIpcProvider } from '@/features/workspace-handoff';
import { createFakeHandoffIpc } from '@/shared/desktop/handoffIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ThreadCloudBadge } from './ThreadCloudBadge';

function mount(status: Parameters<typeof createFakeHandoffIpc>[0]): ReturnType<typeof createFakeHandoffIpc> {
  const ipc = createFakeHandoffIpc(status);
  renderWithTheme(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <HandoffIpcProvider ipc={ipc}>
        <ThreadCloudBadge uuid="u1" />
      </HandoffIpcProvider>
    </QueryClientProvider>,
  );
  return ipc;
}

describe('ThreadCloudBadge', () => {
  it('shows In cloud for a thread running in the cloud', async () => {
    mount({ status: { state: 'CLOUD_IDLE', executor: 'cloud' } });
    expect(await screen.findByTestId('executor-badge')).toHaveTextContent('In cloud');
  });

  it('shows Returning for a thread on its way back', async () => {
    mount({ status: { state: 'RECLAIMING', executor: 'returning' } });
    expect(await screen.findByTestId('executor-badge')).toHaveTextContent('Returning');
  });

  it('shows nothing for a thread that runs here, and asks only from the host\'s own records', async () => {
    const ipc = mount({});
    await waitFor(() => expect(ipc.calls.statuses).toEqual(['u1']));
    expect(screen.queryByTestId('executor-badge')).toBeNull();
  });
});
