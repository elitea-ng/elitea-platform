/**
 * Steering in the workspace composer: while a turn runs, the input stays open, Send queues the text
 * (`onSteer`) instead of starting a turn, and "Stop and send" hands the text to `onStopAndSend`.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { useDesktopLayout } from '@/widgets/desktop-shell';

import type { UnsentSteers } from './useSteering';
import { WorkspaceComposer, type WorkspaceComposerProps } from './WorkspaceComposer';

type Handler = (text: string) => Promise<boolean>;

type OnSend = WorkspaceComposerProps['onSend'];

async function mount(handlers: { onSend: OnSend; onSteer?: Handler; onStopAndSend?: Handler; unsent?: UnsentSteers }, busy = true) {
  useDesktopLayout.getState().setSession({ workspaceId: 'w1', newThread: () => undefined, togglePlanMode: () => undefined });
  const composer = (
    <WorkspaceComposer
      ipc={createFakeWorkspaceIpc()}
      workspaceId="w1"
      canSend
      busy={busy}
      planMode={false}
      onPlanModeChange={() => undefined}
      skills={[]}
      customCommands={[]}
      onSend={handlers.onSend}
      onSteer={handlers.onSteer}
      onStopAndSend={handlers.onStopAndSend}
      unsent={handlers.unsent}
      onStop={() => undefined}
      onCommand={() => undefined}
    />
  );
  const rootRoute = createRootRoute({ component: () => composer });
  const router = createRouter({ routeTree: rootRoute.addChildren([createRoute({ getParentRoute: () => rootRoute, path: '/x', component: () => null })]), history: createMemoryHistory({ initialEntries: ['/'] }) });
  render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return screen.findByRole('textbox');
}

afterEach(() => useDesktopLayout.getState().setSession(null));

describe('WorkspaceComposer steering', () => {
  it('queues a message sent while the turn runs, and clears the input', async () => {
    const onSend = vi.fn<OnSend>().mockResolvedValue(true);
    const onSteer = vi.fn<Handler>().mockResolvedValue(true);
    const input = await mount({ onSend, onSteer, onStopAndSend: vi.fn<Handler>().mockResolvedValue(true) });
    expect(input).not.toBeDisabled();
    await userEvent.type(input, 'also update the tests');
    await userEvent.click(screen.getByTestId('workspace-send-button'));
    await waitFor(() => expect(onSteer).toHaveBeenCalledWith('also update the tests'));
    expect(onSend).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.getByRole('textbox')).toHaveTextContent(''));
  });

  it('offers Stop and send once there is text, and hands the text over', async () => {
    const onStopAndSend = vi.fn<Handler>().mockResolvedValue(true);
    const input = await mount({ onSend: vi.fn<OnSend>(), onSteer: vi.fn<Handler>().mockResolvedValue(true), onStopAndSend });
    const button = screen.getByTestId('workspace-stop-and-send');
    expect(button).toBeDisabled();
    await userEvent.type(input, 'do this instead');
    await waitFor(() => expect(button).not.toBeDisabled());
    await userEvent.click(button);
    await waitFor(() => expect(onStopAndSend).toHaveBeenCalledWith('do this instead'));
  });

  it('shows no Stop and send when the page does not steer', async () => {
    await mount({ onSend: vi.fn<OnSend>() });
    expect(screen.queryByTestId('workspace-stop-and-send')).toBeNull();
  });

  it('puts the unsent texts into the input, and Send again sends the input as the person changed it', async () => {
    const onRetry = vi.fn<Handler>().mockResolvedValue(true);
    const onSend = vi.fn<OnSend>();
    const input = await mount({ onSend, unsent: { id: 1, text: 'first\n\nsecond', onRetry } }, false);
    await waitFor(() => expect(input).toHaveValue('first\n\nsecond'));
    const retry = screen.getByTestId('workspace-steer-retry');
    await waitFor(() => expect(retry).not.toBeDisabled());
    await userEvent.type(input, ' now');
    await userEvent.click(retry);
    await waitFor(() => expect(onRetry).toHaveBeenCalledWith('first\n\nsecond now'));
    expect(onSend).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.getByRole('textbox')).toHaveValue(''));
  });

  it('keeps the input when Send again did not start a turn', async () => {
    const onRetry = vi.fn<Handler>().mockResolvedValue(false);
    const input = await mount({ onSend: vi.fn<OnSend>(), unsent: { id: 1, text: 'later', onRetry } }, false);
    await waitFor(() => expect(input).toHaveValue('later'));
    await userEvent.click(screen.getByTestId('workspace-steer-retry'));
    await waitFor(() => expect(onRetry).toHaveBeenCalledWith('later'));
    expect(input).toHaveValue('later');
  });

  it('shows no Send again while a turn runs', async () => {
    await mount({ onSend: vi.fn<OnSend>(), unsent: { id: 1, text: 'later', onRetry: vi.fn<Handler>() } });
    expect(screen.queryByTestId('workspace-steer-retry')).toBeNull();
  });
});
