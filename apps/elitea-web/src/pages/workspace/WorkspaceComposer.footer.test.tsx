/**
 * The workspace composer's footer and drafts: the unsent text kept per thread
 * (and the steering texts put back still winning), the mode picker
 * ("Act ⌄"), the ⋯ menu and the + that picks images.
 */
import { useState } from 'react';

import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { createFakeWorkspaceIpc, type FakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { clearNamespace } from '@/shared/lib/storage';
import { MenuRow, MenuRowText } from '@/shared/ui/native-menu';
import { useDesktopLayout } from '@/widgets/desktop-shell';

import { threadDraftKey } from './useCarriedDraft';
import { WorkspaceComposer, type WorkspaceComposerProps } from './WorkspaceComposer';

type Extra = Partial<WorkspaceComposerProps> & { ipc?: FakeWorkspaceIpc };

/** The thread `conversationId`'s composer; a thread switch re-mounts it, as the session page does. */
function composerFor(extra: Extra): React.JSX.Element {
  return (
    <WorkspaceComposer
      key={extra.conversationId ?? ''}
      ipc={extra.ipc ?? createFakeWorkspaceIpc()}
      workspaceId="w1"
      canSend
      busy={false}
      planMode={false}
      onPlanModeChange={() => undefined}
      skills={[]}
      customCommands={[]}
      onSend={() => Promise.resolve(true)}
      onStop={() => undefined}
      onCommand={() => undefined}
      {...extra}
    />
  );
}

/** Mounts the composer; `show` switches what it renders (another thread, other props) through a state tick of the router's root. */
async function mount(initial: Extra) {
  useDesktopLayout.getState().setSession({ workspaceId: 'w1', newThread: () => undefined, togglePlanMode: () => undefined });
  let current = initial;
  let rerender: (() => void) | undefined;
  function Host(): React.JSX.Element {
    const [, setTick] = useState(0);
    rerender = () => setTick((n) => n + 1);
    return composerFor(current);
  }
  const rootRoute = createRootRoute({ component: Host });
  const router = createRouter({ routeTree: rootRoute.addChildren([createRoute({ getParentRoute: () => rootRoute, path: '/x', component: () => null })]), history: createMemoryHistory({ initialEntries: ['/'] }) });
  render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  await screen.findByRole('textbox');
  return {
    show: (next: Extra) => {
      current = next;
      act(() => rerender?.());
      return screen.findByRole('textbox');
    },
  };
}

const storedDrafts = (): Record<string, { text: string }> => JSON.parse(window.localStorage.getItem('el.composer.drafts') ?? '{}') as Record<string, { text: string }>;

beforeEach(() => clearNamespace());
afterEach(() => useDesktopLayout.getState().setSession(null));

describe('WorkspaceComposer drafts', () => {
  it('keeps each thread’s unsent text across a switch and back', async () => {
    const user = userEvent.setup();
    const view = await mount({ conversationId: 'c1' });
    await user.type(screen.getByRole('textbox'), 'half a thought');

    const other = await view.show({ conversationId: 'c2' });
    // The other thread has nothing of its own: c1's text does not follow.
    expect(other).toHaveValue('');
    expect(storedDrafts()[threadDraftKey('w1', 'c1')]?.text).toBe('half a thought');
    await user.type(other, 'for c2');

    const back = await view.show({ conversationId: 'c1' });
    expect(back).toHaveValue('half a thought');
    expect(storedDrafts()[threadDraftKey('w1', 'c2')]?.text).toBe('for c2');
    // A new thread is a key of its own.
    expect(threadDraftKey('w1', '')).toBe('workspace:w1:new');
  });

  it('puts the steering texts that did not start a turn back over the restored draft', async () => {
    window.localStorage.setItem('el.composer.drafts', JSON.stringify({ [threadDraftKey('w1', 'c1')]: { text: 'old draft', at: Date.now() } }));
    const unsent = { id: 1, text: 'also run the tests', onRetry: vi.fn().mockResolvedValue(true) };
    await mount({ conversationId: 'c1', unsent });
    // The draft comes back first; the unsent text is put in after it, and neither is lost.
    await waitFor(() => expect(screen.getByRole('textbox')).toHaveValue('also run the tests\n\nold draft'));
    expect(screen.getByTestId('workspace-steer-retry')).toBeEnabled();
  });
});

describe('WorkspaceComposer mode picker', () => {
  it('shows the mode, offers Act, Plan and Bypass with a check on the current one, and hands back the pick', async () => {
    const onPlanModeChange = vi.fn();
    const onBypassChange = vi.fn();
    const user = userEvent.setup();
    await mount({ onPlanModeChange, onBypassChange });
    const picker = screen.getByTestId('mode-picker');
    expect(picker).toHaveTextContent('Act');

    await user.click(picker);
    const menu = await screen.findByRole('menu', { name: 'Mode' });
    const rows = within(menu).getAllByRole('menuitemradio');
    expect(rows.map((row) => row.textContent)).toEqual(['ActEdits with approvals', 'PlanPlans, changes nothing', 'BypassNo approvals']);
    expect(rows.map((row) => row.getAttribute('aria-checked'))).toEqual(['true', 'false', 'false']);
    await user.click(within(menu).getByRole('menuitemradio', { name: 'Plan mode (no changes)' }));
    expect(onPlanModeChange).toHaveBeenCalledWith(true);
    expect(onBypassChange).toHaveBeenCalledWith(false);

    // Bypass only asks: the page turns it on (after its warning) and leaves plan mode then.
    onPlanModeChange.mockClear();
    await user.click(picker);
    await user.click(await screen.findByRole('menuitemradio', { name: 'Bypass mode (no approvals)' }));
    expect(onBypassChange).toHaveBeenLastCalledWith(true);
    expect(onPlanModeChange).not.toHaveBeenCalled();
  });

  it('shows the current mode checked, offers no Bypass without the page’s say, and none of it for the cloud copy', async () => {
    const user = userEvent.setup();
    const view = await mount({ planMode: true });
    expect(screen.getByTestId('mode-picker')).toHaveTextContent('Plan');
    await user.click(screen.getByTestId('mode-picker'));
    const rows = within(await screen.findByRole('menu', { name: 'Mode' })).getAllByRole('menuitemradio');
    expect(rows.map((row) => row.getAttribute('aria-label'))).toEqual(['Act mode (edits with approvals)', 'Plan mode (no changes)']);
    expect(rows[1]).toHaveAttribute('aria-checked', 'true');
    await user.keyboard('{Escape}');

    await view.show({ showMode: false });
    expect(screen.queryByTestId('mode-picker')).toBeNull();
  });
});

describe('WorkspaceComposer footer', () => {
  it('offers ⋯ only with rows for it, and closes it when a row is picked', async () => {
    const user = userEvent.setup();
    const picked = vi.fn();
    const view = await mount({});
    expect(screen.queryByTestId('workspace-composer-more')).toBeNull();

    await view.show({
      more: (close) => (
        <MenuRow
          onClick={() => {
            close();
            picked();
          }}
        >
          <MenuRowText>Step limit</MenuRowText>
        </MenuRow>
      ),
    });
    await user.click(screen.getByTestId('workspace-composer-more'));
    await user.click(await screen.findByRole('menuitem', { name: 'Step limit' }));
    expect(picked).toHaveBeenCalledOnce();
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());
  });

  it('attaches images picked with +, and offers no + where images are not taken', async () => {
    const ipc = createFakeWorkspaceIpc();
    const view = await mount({ ipc });
    const file = new File([new Uint8Array([137, 80, 78, 71])], 'picked.png', { type: 'image/png' });
    const text = new File(['notes'], 'notes.txt', { type: 'text/plain' });
    fireEvent.change(screen.getByTestId('workspace-attach-images-input'), { target: { files: [file, text] } });
    await waitFor(() => expect(screen.getAllByTestId('image-thumb')).toHaveLength(1));
    expect(ipc.calls.attachedBytes).toEqual([expect.objectContaining({ workspaceId: 'w1', mime: 'image/png', name: 'picked.png' })]);

    await view.show({ ipc, acceptImages: false });
    expect(screen.queryByTestId('workspace-attach-images')).toBeNull();
  });

  it('turns Send into Stop while a turn runs, also while Send steers it and the input is empty', async () => {
    const onStop = vi.fn();
    const user = userEvent.setup();
    const view = await mount({ busy: true, canSend: false, onStop });
    expect(screen.queryByTestId('workspace-send-button')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Stop generating' }));
    expect(onStop).toHaveBeenCalledOnce();

    await view.show({ busy: true, onStop, onSteer: vi.fn().mockResolvedValue(true) });
    expect(screen.queryByTestId('workspace-send-button')).toBeNull();
    expect(screen.getAllByRole('button', { name: 'Stop generating' })).toHaveLength(1);
  });
});
