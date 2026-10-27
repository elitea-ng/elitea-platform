import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { createFakeWorktreeIpc, type FakeWorktreeIpc } from '@/shared/desktop/worktreeIpc.fake';

import { StartInWorktreeError, useStartInWorktree } from './useStartInWorktree';
import { holdFirstMessage, initialWorkIn, listenForWorktree, preferWorktree, takeFirstMessage } from './workIn';

function start(ipc: FakeWorktreeIpc) {
  const client = new QueryClient();
  const { result } = renderHook(() => useStartInWorktree(ipc), {
    wrapper: ({ children }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>,
  });
  if (result.current === undefined) throw new Error('no host');
  return result.current;
}

describe('useStartInWorktree', () => {
  it('makes a task worktree of the folder for a fresh thread UUID', async () => {
    const ipc = createFakeWorktreeIpc();
    const entry = await start(ipc)('w1');
    expect(entry.branch).toMatch(/^elitea\//);
    const [workspaceId, uuid] = ipc.calls.create[0] as [string, string];
    expect(workspaceId).toBe('w1');
    expect(uuid).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  });

  it("says why a worktree could not be made: the host's reason, never a guess about ownership", async () => {
    const ipc = createFakeWorktreeIpc();
    const reason = '/work/repo/.git/config sets `core.hookspath` (a hooks path)';
    ipc.failNext('create', 'unsafe_repository', reason);
    const failure = await start(ipc)('w1').catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(StartInWorktreeError);
    expect((failure as Error).message).toBe(`Parallel tasks cannot use this repository: ${reason}`);
  });
});

describe('workIn hand-offs', () => {
  it('passes the first message to the worktree thread once', () => {
    holdFirstMessage('wt-1', ['fix the build', []]);
    expect(takeFirstMessage('wt-1')).toEqual(['fix the build', []]);
    expect(takeFirstMessage('wt-1')).toBeUndefined();
  });

  it('opens the next new thread on "New worktree", or switches the one on screen', () => {
    preferWorktree('f1');
    expect(initialWorkIn('f1')).toBe('worktree');
    expect(initialWorkIn('f1')).toBe('folder');
    let switched = 0;
    const stop = listenForWorktree('f2', () => switched++);
    preferWorktree('f2');
    expect(switched).toBe(1);
    stop();
    expect(initialWorkIn('f2')).toBe('folder');
  });
});
