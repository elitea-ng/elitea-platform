import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { preferWorktree, rememberWorkIn, takeFirstMessage } from '@/features/workspace-worktrees';
import { createFakeWorktreeIpc } from '@/shared/desktop/worktreeIpc.fake';

import type { SelectionSeed } from './useAgentSelection';
import type { HandedFirstMessage } from './useFirstSend';
import { useWorkIn } from './useWorkIn';

const navigate = vi.fn();
const fake = createFakeWorktreeIpc();

function renderWorkIn(workspaceId: string, offered: boolean, send = vi.fn(() => Promise.resolve(true)), onError = vi.fn(), handOver?: () => SelectionSeed | undefined) {
  const client = new QueryClient();
  const hook = renderHook(() => useWorkIn({ workspaceId, offered, send, handOver, onError, open: navigate, ipc: fake }), {
    wrapper: ({ children }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>,
  });
  return { ...hook, send, onError };
}

describe('useWorkIn', () => {
  afterEach(() => navigate.mockReset());

  it('proposes a new worktree by default; choosing the folder sends there and is remembered for that folder', async () => {
    const { result, send } = renderWorkIn('f1', true);
    expect(result.current.value).toBe('worktree');
    act(() => result.current.choose('folder'));
    expect(result.current.value).toBe('folder');
    await result.current.send('hello', [], []);
    expect(send).toHaveBeenCalledWith('hello', [], []);
    expect(navigate).not.toHaveBeenCalled();
    // The next new thread of the same folder proposes the folder; another folder still proposes a worktree.
    expect(renderWorkIn('f1', true).result.current.value).toBe('folder');
    expect(renderWorkIn('f1-other', true).result.current.value).toBe('worktree');
  });

  it('remembers a switch back to "New worktree" too', () => {
    rememberWorkIn('f5', 'folder');
    const { result } = renderWorkIn('f5', true);
    expect(result.current.value).toBe('folder');
    act(() => result.current.choose('worktree'));
    expect(renderWorkIn('f5', true).result.current.value).toBe('worktree');
  });

  it('keeps the proposal when the folder is offered a moment after the thread opens', () => {
    const client = new QueryClient();
    const { result, rerender } = renderHook(({ offered }) => useWorkIn({ workspaceId: 'f6', offered, send: vi.fn(() => Promise.resolve(true)), onError: vi.fn(), open: navigate, ipc: fake }), {
      initialProps: { offered: false },
      wrapper: ({ children }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>,
    });
    expect(result.current.value).toBe('folder');
    rerender({ offered: true });
    expect(result.current.value).toBe('worktree');
  });

  it('makes the worktree on send, hands it the message and opens its thread', async () => {
    const { result, send } = renderWorkIn('f2', true);
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

  it('a failed hand-off is retried on the same thread id, and the next message gets a new one (#3077)', async () => {
    const ipc = createFakeWorktreeIpc();
    const client = new QueryClient();
    const { result } = renderHook(() => useWorkIn({ workspaceId: 'f8', offered: true, send: vi.fn(() => Promise.resolve(true)), onError: vi.fn(), open: navigate, ipc }), {
      wrapper: ({ children }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>,
    });
    ipc.failNext('create', 'worktree_failed');
    await act(async () => {
      expect(await result.current.send('fix the build', [], [])).toBe(false);
    });
    await act(async () => {
      expect(await result.current.send('fix the build', [], [])).toBe(true);
    });
    await act(async () => {
      expect(await result.current.send('and the docs', [], [])).toBe(true);
    });
    const uuids = ipc.calls.create.map((call) => call[1]);
    expect(uuids[0]).toBe(uuids[1]);
    expect(uuids[2]).not.toBe(uuids[1]);
  });

  it('hands the worktree\'s thread the agents chosen for the new thread with its message (#3128)', async () => {
    const team = { id: 3, name: 'Review crew', lead: true, primary: { applicationId: 6, versionId: 10, name: 'Reviewer' }, others: [{ applicationId: 5, versionId: 9, name: 'Coder' }] };
    const seed: SelectionSeed = { projectId: 42, agentId: '6', versionId: '10', companions: [{ agentId: '5', name: 'Coder', versionId: 9, agentType: '', cloud: false }], team };
    const { result, send } = renderWorkIn('f9', true, vi.fn(() => Promise.resolve(true)), vi.fn(), () => seed);
    act(() => result.current.choose('worktree'));
    await act(async () => {
      expect(await result.current.send('fix the build', [], [])).toBe(true);
    });
    expect(send).not.toHaveBeenCalled();
    const [entry] = await fake.list('f9');
    const handed = takeFirstMessage<HandedFirstMessage>(entry?.workspace_id ?? '');
    expect(handed).toEqual({ args: ['fix the build', [], []], seed });
  });

  it('opens on "New worktree" from the folder menu, whatever was remembered; never offered in a worktree itself', () => {
    rememberWorkIn('f3', 'folder');
    preferWorktree('f3');
    expect(renderWorkIn('f3', true).result.current.value).toBe('worktree');
    const inside = renderWorkIn('wt-1', false).result.current;
    expect(inside.offered).toBe(false);
    expect(inside.value).toBe('folder');
  });

  it('says why the worktree could not be made and sends nothing', async () => {
    fake.failNext('create', 'not_a_git_repository', 'not the top');
    const { result, send, onError } = renderWorkIn('f4', true);
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
