/**
 * Images in the workspace composer (#1229): a pasted image and a dropped file
 * are attached through the host (the fake), shown as thumbnails with a remove
 * button, carried by Send, and refused with the host's reason.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { createFakeWorkspaceIpc, type FakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { useDesktopLayout } from '@/widgets/desktop-shell';

import { WorkspaceComposer } from './WorkspaceComposer';

async function mount(ipc: FakeWorkspaceIpc, onSend = vi.fn().mockResolvedValue(true), extra: { acceptImages?: boolean } = {}) {
  useDesktopLayout.getState().setSession({ workspaceId: 'w1', newThread: () => undefined, togglePlanMode: () => undefined });
  const composer = (
    <WorkspaceComposer
      ipc={ipc}
      workspaceId="w1"
      canSend
      busy={false}
      planMode={false}
      onPlanModeChange={() => undefined}
      skills={[]}
      customCommands={[]}
      onSend={onSend}
      onStop={() => undefined}
      onCommand={() => undefined}
      {...extra}
    />
  );
  const rootRoute = createRootRoute({ component: () => composer });
  const router = createRouter({ routeTree: rootRoute.addChildren([createRoute({ getParentRoute: () => rootRoute, path: '/x', component: () => null })]), history: createMemoryHistory({ initialEntries: ['/'] }) });
  render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  await screen.findByRole('textbox');
  return onSend;
}

function pasteImage(target: HTMLElement, name = 'shot.png'): void {
  const file = new File([new Uint8Array([137, 80, 78, 71])], name, { type: 'image/png' });
  fireEvent.paste(target, { clipboardData: { files: [file], items: [], types: ['Files'] } });
}

afterEach(() => useDesktopLayout.getState().setSession(null));

describe('WorkspaceComposer images', () => {
  it('attaches a pasted image, shows its thumbnail and sends its token with the prompt', async () => {
    const ipc = createFakeWorkspaceIpc();
    const onSend = await mount(ipc);
    pasteImage(screen.getByRole('textbox'));
    await waitFor(() => expect(screen.getAllByTestId('image-thumb')).toHaveLength(1));
    expect(ipc.calls.attachedBytes).toHaveLength(1);
    expect(ipc.calls.attachedBytes[0]).toMatchObject({ workspaceId: 'w1', mime: 'image/png', name: 'shot.png' });
    await userEvent.type(screen.getByRole('textbox'), 'what is wrong in this dialog?');
    await userEvent.click(screen.getByTestId('workspace-send-button'));
    await waitFor(() => expect(onSend).toHaveBeenCalled());
    const images = onSend.mock.calls[0]?.[4] as { picks: { source: string; token: string }[]; shown: { name: string }[] };
    expect(images.picks).toEqual([{ source: 'clipboard', token: 'att-1' }]);
    expect(images.shown[0]?.name).toBe('shot.png');
    // Sent: the thumbnails are used up.
    await waitFor(() => expect(screen.queryAllByTestId('image-thumb')).toHaveLength(0));
  });

  it('leaves a text paste to the input and attaches nothing', async () => {
    const ipc = createFakeWorkspaceIpc();
    await mount(ipc);
    fireEvent.paste(screen.getByRole('textbox'), { clipboardData: { files: [], items: [], types: ['text/plain'], getData: () => 'hello' } });
    expect(ipc.calls.attachedBytes).toHaveLength(0);
    expect(screen.queryByTestId('image-strip')).toBeNull();
  });

  it('takes a file dropped on the window through the session, and removing it tells the host', async () => {
    const ipc = createFakeWorkspaceIpc();
    await mount(ipc);
    const attach = useDesktopLayout.getState().session?.attachFiles;
    expect(attach).toBeDefined();
    act(() => attach?.(['/Users/me/Desktop/mock.png']));
    await waitFor(() => expect(screen.getAllByTestId('image-thumb')).toHaveLength(1));
    expect(ipc.calls.attachedFiles).toEqual([{ workspaceId: 'w1', path: '/Users/me/Desktop/mock.png' }]);
    await userEvent.click(within(screen.getByTestId('image-strip')).getByRole('button', { name: /mock\.png/ }));
    expect(screen.queryAllByTestId('image-thumb')).toHaveLength(0);
    expect(ipc.calls.removedAttachments).toEqual([{ workspaceId: 'w1', token: 'att-1' }]);
  });

  it('says why the host refused an image, and keeps no thumbnail for it', async () => {
    const ipc = createFakeWorkspaceIpc();
    await mount(ipc);
    ipc.failNext('attachFile', 'path_denied', 'That file is protected and cannot be attached.');
    act(() => useDesktopLayout.getState().session?.attachFiles?.(['/Users/me/.ssh/id_rsa']));
    expect(await screen.findByTestId('attachment-error')).toHaveTextContent('protected');
    expect(screen.queryByTestId('image-strip')).toBeNull();
  });

  it('offers no images for a thread in the cloud', async () => {
    const ipc = createFakeWorkspaceIpc();
    await mount(ipc, undefined, { acceptImages: false });
    expect(useDesktopLayout.getState().session?.attachFiles).toBeUndefined();
    pasteImage(screen.getByRole('textbox'));
    expect(ipc.calls.attachedBytes).toHaveLength(0);
  });

  it('attaches at most five', async () => {
    const ipc = createFakeWorkspaceIpc();
    await mount(ipc);
    const paths = Array.from({ length: 7 }, (_, index) => `/tmp/${String(index)}.png`);
    act(() => useDesktopLayout.getState().session?.attachFiles?.(paths));
    await waitFor(() => expect(screen.getAllByTestId('image-thumb')).toHaveLength(5));
    expect(ipc.calls.attachedFiles).toHaveLength(5);
  });
});
