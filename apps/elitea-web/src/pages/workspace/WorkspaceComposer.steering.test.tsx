/**
 * Steering in the workspace composer: while a turn runs, the input stays open, the one round button is Stop until
 * there is text and Send then, which queues the text (`onSteer`) instead of starting a turn; the queued messages sit
 * at the top of the composer (`queue`) with Send now, Edit (back into the input) and Remove.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { useDesktopLayout } from '@/widgets/desktop-shell';

import type { SteeringQueueBinding } from './SteeringQueue';
import type { UnsentSteers } from './useSteering';
import { WorkspaceComposer, type WorkspaceComposerProps } from './WorkspaceComposer';

type Handler = (text: string) => Promise<boolean>;

type OnSend = WorkspaceComposerProps['onSend'];

interface Handlers {
  onSend: OnSend;
  onSteer?: Handler;
  queue?: SteeringQueueBinding;
  unsent?: UnsentSteers;
  onStop?: () => void;
}

async function mount(handlers: Handlers, busy = true) {
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
      queue={handlers.queue}
      unsent={handlers.unsent}
      onStop={handlers.onStop ?? (() => undefined)}
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
    const input = await mount({ onSend, onSteer });
    expect(input).not.toBeDisabled();
    await userEvent.type(input, 'also update the tests');
    await userEvent.click(screen.getByTestId('workspace-send-button'));
    await waitFor(() => expect(onSteer).toHaveBeenCalledWith('also update the tests'));
    expect(onSend).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.getByRole('textbox')).toHaveTextContent(''));
  });

  it('draws one round button: Stop while the input is empty, Send once there is text', async () => {
    const onStop = vi.fn();
    const input = await mount({ onSend: vi.fn<OnSend>(), onSteer: vi.fn<Handler>().mockResolvedValue(true), onStop });
    expect(screen.queryByTestId('workspace-send-button')).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Stop generating' }));
    expect(onStop).toHaveBeenCalledOnce();
    await userEvent.type(input, 'do this too');
    await waitFor(() => expect(screen.getByTestId('workspace-send-button')).not.toBeDisabled());
    expect(screen.queryByTestId('workspace-stop-button')).toBeNull();
  });

  it('keeps Stop whatever the input holds when the page does not steer', async () => {
    const input = await mount({ onSend: vi.fn<OnSend>() });
    await userEvent.type(input, 'later');
    expect(screen.getByTestId('workspace-stop-button')).toBeInTheDocument();
    expect(screen.queryByTestId('workspace-send-button')).toBeNull();
  });

  it('lists the queued messages: Send now and Remove by id, Edit puts the text back before what is typed', async () => {
    const queue: SteeringQueueBinding = {
      pending: [
        { steerId: 's1', text: 'also run the tests', revision: 1 },
        { steerId: 's2', text: 'and the docs', revision: 1 },
      ],
      error: null,
      remove: vi.fn<SteeringQueueBinding['remove']>().mockResolvedValue(true),
      sendNow: vi.fn<SteeringQueueBinding['sendNow']>().mockResolvedValue(undefined),
    };
    const input = await mount({ onSend: vi.fn<OnSend>(), onSteer: vi.fn<Handler>().mockResolvedValue(true), queue });
    const rows = within(screen.getByTestId('steering-queue')).getAllByTestId('steer-pending');
    expect(rows.map((row) => row.textContent)).toEqual(['also run the tests', 'and the docs']);
    await userEvent.click(within(rows[1] as HTMLElement).getByRole('button', { name: 'Send now' }));
    expect(queue.sendNow).toHaveBeenCalledWith('s2');
    await userEvent.click(within(rows[1] as HTMLElement).getByRole('button', { name: 'Remove' }));
    expect(queue.remove).toHaveBeenCalledWith('s2');
    await userEvent.type(input, 'typed');
    await userEvent.click(within(rows[0] as HTMLElement).getByRole('button', { name: 'Edit' }));
    expect(queue.remove).toHaveBeenLastCalledWith('s1');
    await waitFor(() => expect(input).toHaveValue('also run the tests\n\ntyped'));
  });

  it('keeps the input as it is when Edit could not take the message off the queue', async () => {
    const queue: SteeringQueueBinding = {
      pending: [{ steerId: 's1', text: 'taken already', revision: 1 }],
      error: 'The agent has it already.',
      remove: vi.fn<SteeringQueueBinding['remove']>().mockResolvedValue(false),
      sendNow: vi.fn<SteeringQueueBinding['sendNow']>(),
    };
    const input = await mount({ onSend: vi.fn<OnSend>(), onSteer: vi.fn<Handler>().mockResolvedValue(true), queue });
    await userEvent.click(screen.getByRole('button', { name: 'Edit' }));
    await waitFor(() => expect(queue.remove).toHaveBeenCalledWith('s1'));
    expect(input).toHaveValue('');
    expect(screen.getByTestId('steer-error')).toHaveTextContent('The agent has it already.');
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
