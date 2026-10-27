import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { holdFirstMessage, preferWorktree } from '@/features/workspace-worktrees';
import { createFakeWorktreeIpc } from '@/shared/desktop/worktreeIpc.fake';

import { useWorkIn } from './useWorkIn';

const navigate = vi.fn();
const fake = createFakeWorktreeIpc();

function renderWorkIn(workspaceId: string, offered: boolean, canSend: boolean, send = vi.fn(() => Promise.resolve(true)), onError = vi.fn()) {
  const client = new QueryClient();
  const hook = renderHook(({ ready }) => useWorkIn({ workspaceId, offered, canSend: ready, send, onError, open: navigate, ipc: fake }), {
    initialProps: { ready: canSend },
    wrapper: ({ children }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>,
  });
  return { ...hook, send, onError };
}

describe('useWorkIn', () => {
  afterEach(() => navigate.mockReset());

  it('sends in the folder until "New worktree" is chosen', async () => {
    const { result, send } = renderWorkIn('f1', true, true);
    expect(result.current.value).toBe('folder');
    await result.current.send('hello', [], []);
    expect(send).toHaveBeenCalledWith('hello', [], []);
    expect(navigate).not.toHaveBeenCalled();
  });

  it('makes the worktree on send, hands it the message and opens its thread', async () => {
    const { result, send } = renderWorkIn('f2', true, true);
    act(() => result.current.choose('worktree'));
    let sent = false;
    await act(async () => {
      sent = await result.current.send('fix the build', [], []);
    });
    expect(sent).toBe(true);
    expect(send).not.toHaveBeenCalled();
    const [entry] = await fake.list('f2');
    expect(navigate).toHaveBeenCalledWith(entry?.workspace_id);
  });

  it('the worktree thread sends the held message once it can', async () => {
    holdFirstMessage('wt-9', ['fix the build', []]);
    const { rerender, send } = renderWorkIn('wt-9', false, false);
    expect(send).not.toHaveBeenCalled();
    rerender({ ready: true });
    await waitFor(() => expect(send).toHaveBeenCalledWith('fix the build', []));
    rerender({ ready: true });
    expect(send).toHaveBeenCalledTimes(1);
  });

  it('opens on "New worktree" from the folder menu; never offered in a worktree itself', () => {
    preferWorktree('f3');
    expect(renderWorkIn('f3', true, true).result.current.value).toBe('worktree');
    const inside = renderWorkIn('wt-1', false, true).result.current;
    expect(inside.offered).toBe(false);
  });

  it('says why the worktree could not be made and sends nothing', async () => {
    fake.failNext('create', 'not_a_git_repository', 'not the top');
    const { result, send, onError } = renderWorkIn('f4', true, true);
    act(() => result.current.choose('worktree'));
    let sent = true;
    await act(async () => {
      sent = await result.current.send('x', [], []);
    });
    expect(sent).toBe(false);
    expect(send).not.toHaveBeenCalled();
    expect(onError).toHaveBeenCalledTimes(1);
    expect(navigate).not.toHaveBeenCalled();
  });
});
