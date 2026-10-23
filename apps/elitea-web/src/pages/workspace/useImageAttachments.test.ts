/**
 * The composer's images belong to one workspace (#1229): moving to another,
 * or the composer going away, lets go of the ones not sent (the host forgets
 * their tokens), and one still on its way when that happens is let go when it
 * lands instead of shown.
 */
import { act, renderHook, waitFor } from '@testing-library/react';
import type { ClipboardEvent } from 'react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { useDesktopLayout } from '@/widgets/desktop-shell';

import { useImageAttachments } from './useImageAttachments';

function paste(): ClipboardEvent<HTMLElement> {
  const file = new File([new Uint8Array([137, 80, 78, 71])], 'shot.png', { type: 'image/png' });
  return { clipboardData: { files: [file] }, preventDefault: () => undefined } as unknown as ClipboardEvent<HTMLElement>;
}

beforeEach(() => useDesktopLayout.getState().setSession({ workspaceId: 'w1', newThread: () => undefined, togglePlanMode: () => undefined }));
afterEach(() => useDesktopLayout.getState().setSession(null));

describe('useImageAttachments', () => {
  it('lets go of the unsent images when the workspace changes and when it unmounts', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result, rerender, unmount } = renderHook(({ workspaceId }) => useImageAttachments(ipc, workspaceId, true), { initialProps: { workspaceId: 'w1' } });
    act(() => result.current.onPaste(paste()));
    await waitFor(() => expect(result.current.images).toHaveLength(1));
    const first = result.current.images[0]?.token;

    rerender({ workspaceId: 'w2' });
    expect(result.current.images).toHaveLength(0);
    expect(ipc.calls.removedAttachments).toEqual([{ workspaceId: 'w1', token: first }]);

    // The limit starts again for the new workspace: five fit.
    for (let n = 0; n < 5; n += 1) act(() => result.current.onPaste(paste()));
    await waitFor(() => expect(result.current.images).toHaveLength(5));
    const held = result.current.images.map((image) => image.token);

    unmount();
    expect(ipc.calls.removedAttachments.slice(1)).toEqual(held.map((token) => ({ workspaceId: 'w2', token })));
  });

  it('lets go of an image that lands after the workspace changed', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result, rerender } = renderHook(({ workspaceId }) => useImageAttachments(ipc, workspaceId, true), { initialProps: { workspaceId: 'w1' } });
    act(() => result.current.onPaste(paste()));
    // Still being read when the composer moves on.
    rerender({ workspaceId: 'w2' });
    await waitFor(() => expect(ipc.calls.removedAttachments).toHaveLength(1));
    expect(ipc.calls.removedAttachments[0]?.workspaceId).toBe('w1');
    expect(result.current.images).toHaveLength(0);
  });

  it('does not let go of images a sent message used up', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result, unmount } = renderHook(() => useImageAttachments(ipc, 'w1', true));
    act(() => result.current.onPaste(paste()));
    await waitFor(() => expect(result.current.images).toHaveLength(1));
    act(() => result.current.clear());
    unmount();
    expect(ipc.calls.removedAttachments).toEqual([]);
  });
});
