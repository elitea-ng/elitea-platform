import { describe, expect, it, vi } from 'vitest';

import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { fireEvent, screen, waitFor } from '@testing-library/react';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { NormalizedNotification } from '../api/normalize';
import { NotificationMessage } from './NotificationMessage';

function row(meta: NormalizedNotification['meta'], projectId?: string): NormalizedNotification {
  return { id: '1', eventType: 'run_finished', createdAt: '2026-11-03T00:00:00Z', isSeen: false, ...(meta === undefined ? {} : { meta }), ...(projectId === undefined ? {} : { projectId }) };
}

/** The row inside a real router, with the two places it can open (ROUTE-070 and the folder list). */
function mount(notification: NormalizedNotification, onClose?: () => void) {
  const rootRoute = createRootRoute({
    component: () => (
      <>
        <NotificationMessage
          notification={notification}
          onCloseNotificationList={onClose}
        />
        <Outlet />
      </>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/', component: () => null }),
      createRoute({ getParentRoute: () => rootRoute, path: '/$projectId/$', component: () => null }),
      createRoute({ getParentRoute: () => rootRoute, path: '/workspaces', component: () => null }),
    ]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  });
  renderWithTheme(<RouterProvider router={router} />);
  return router;
}

describe('the bell row of a run_finished notification', () => {
  it.each([
    ['completed', 'Run finished', 'A run is done.'],
    ['failed', 'Run failed', 'A run stopped with an error.'],
    ['waiting', 'Run needs you', 'A run waits for your answer.'],
    ['limit_reached', 'Run stopped at its limit', 'A run reached its chain limit.'],
  ])('%s reads "%s %s"', async (outcome, title, body) => {
    mount(row({ outcome, origin: 'cloud', conversationUuid: 'c-1', projectId: '9' }));
    expect(await screen.findByTestId('run-finished-message')).toHaveTextContent(`${title} ${body}`);
  });

  it('opens the conversation in its project and closes the list', async () => {
    const close = vi.fn();
    const router = mount(row({ outcome: 'completed', origin: 'cloud', conversationUuid: 'c-1', projectId: '9' }, '1'), close);
    fireEvent.click(await screen.findByRole('button', { name: 'Open the conversation' }));
    expect(close).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(router.state.location.href).toBe('/9/chat?conversation=c-1'));
  });

  it('has no link for a desktop-only thread in a browser', async () => {
    mount(row({ outcome: 'completed', origin: 'desktop', threadId: 't-1' }, '1'));
    expect(await screen.findByTestId('run-finished-message')).toHaveTextContent('Run finished A run is done.');
    expect(screen.queryByRole('button')).toBeNull();
  });

  it('never shows a message the row may carry: the text is fixed', async () => {
    mount(row({ outcome: 'completed', message: 'secret prompt text', conversationUuid: 'c-1', projectId: '9' }));
    await screen.findByTestId('run-finished-message');
    expect(screen.queryByText(/secret prompt text/)).toBeNull();
  });
});
