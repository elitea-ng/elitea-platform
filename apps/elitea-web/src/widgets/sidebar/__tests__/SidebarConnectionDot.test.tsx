import { act, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { createNoopSocketClient, SocketClientContext } from '@/shared/api/socket/client';
import { createRealtimeStatusStore, RealtimeStatusContext, type RealtimeStatusStore } from '@/shared/api/sse';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { SidebarConnectionDot } from '../ui/SidebarConnectionDot';

function renderDot(store: RealtimeStatusStore) {
  return renderWithTheme(
    // The noop socket.io client — permanently `disconnected` — is what every
    // deployment provides. It must have no say in the dot.
    <SocketClientContext.Provider value={createNoopSocketClient()}>
      <RealtimeStatusContext.Provider value={store}>
        <SidebarConnectionDot />
      </RealtimeStatusContext.Provider>
    </SocketClientContext.Provider>,
  );
}

describe('SidebarConnectionDot (SHELL-012)', () => {
  it('renders nothing while no live channel is subscribed', () => {
    const { container } = renderDot(createRealtimeStatusStore());
    expect(container).toBeEmptyDOMElement();
  });

  it('renders nothing with no status provider mounted (admin bundle, isolated renders)', () => {
    const { container } = renderWithTheme(<SidebarConnectionDot />);
    expect(container).toBeEmptyDOMElement();
  });

  it('shows "Connected" when the SSE channel is open, even under the permanently-disconnected noop socket.io client', () => {
    const store = createRealtimeStatusStore();
    store.report('notifications', 'open');
    renderDot(store);
    const dot = screen.getByTestId('sidebar-connection-dot');
    expect(dot).toHaveAttribute('aria-label', 'Connected');
    expect(dot).toHaveAttribute('data-status', 'connected');
    expect(screen.queryByLabelText('Disconnected')).not.toBeInTheDocument();
  });

  it.each([
    ['connecting', 'Connecting…'],
    ['reconnecting', 'Connection lost — reconnecting…'],
    ['offline', 'Offline — live updates unavailable'],
  ] as const)('labels the %s state "%s"', (state, label) => {
    const store = createRealtimeStatusStore();
    store.report('notifications', state);
    renderDot(store);
    expect(screen.getByTestId('sidebar-connection-dot')).toHaveAttribute('aria-label', label);
  });

  it('updates its tooltip as the channel recovers', () => {
    const store = createRealtimeStatusStore();
    store.report('notifications', 'reconnecting');
    renderDot(store);
    expect(screen.getByTestId('sidebar-connection-dot')).toHaveAttribute('aria-label', 'Connection lost — reconnecting…');
    act(() => store.report('notifications', 'open'));
    expect(screen.getByTestId('sidebar-connection-dot')).toHaveAttribute('aria-label', 'Connected');
  });
});
