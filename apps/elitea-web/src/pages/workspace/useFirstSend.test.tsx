/**
 * The new thread's first message on screen before its turn (#3127): the worktree's thread shows the message its
 * folder's thread handed it at once, sends it once it can, once; a failed send keeps it with Retry.
 */
import { act, renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { holdFirstMessage, peekFirstMessage } from '@/features/workspace-worktrees';

import { peekHanded, useFirstMessageSlot, useFirstSend, type HandedFirstMessage } from './useFirstSend';

function renderFirst(workspaceId: string, canSend: boolean, send: ReturnType<typeof vi.fn>) {
  return renderHook(
    ({ ready }) => {
      const slot = useFirstMessageSlot(peekHanded(workspaceId));
      const first = useFirstSend({ workspaceId, slot, fresh: true, canSend: ready, send: send as never });
      return { slot, first };
    },
    { initialProps: { ready: canSend } },
  );
}

describe('useFirstSend', () => {
  it('shows the handed message from the first render and sends it once the thread can, once', async () => {
    const handed: HandedFirstMessage = { args: ['fix the build', [], []] };
    holdFirstMessage('wt-1', handed);
    const send = vi.fn(() => Promise.resolve(true));
    const { result, rerender } = renderFirst('wt-1', false, send);
    expect(result.current.slot.message).toEqual({ args: ['fix the build', [], []], state: 'waiting' });
    expect(result.current.first.holding).toBe(true);
    expect(send).not.toHaveBeenCalled();

    rerender({ ready: true });
    rerender({ ready: true });
    await waitFor(() => expect(result.current.slot.message).toBeNull());
    expect(send).toHaveBeenCalledTimes(1);
    expect(send).toHaveBeenCalledWith('fix the build', [], []);
    // Sent: the hand-off is dropped.
    expect(peekFirstMessage('wt-1')).toBeUndefined();
  });

  it('a fresh thread\'s send resolves at once and runs behind; a second send waits; a failure keeps it with Retry', async () => {
    let answer: (sent: boolean) => void = () => undefined;
    const send = vi.fn(() => new Promise<boolean>((resolve) => { answer = resolve; }));
    const { result } = renderFirst('f-1', true, send);

    let captured = false;
    await act(async () => {
      captured = await result.current.first.send('hello', [], []);
    });
    expect(captured).toBe(true);
    expect(result.current.slot.message?.state).toBe('preparing');
    await act(async () => {
      expect(await result.current.first.send('again', [], [])).toBe(false);
    });
    expect(send).toHaveBeenCalledTimes(1);

    await act(() => Promise.resolve(answer(false)));
    expect(result.current.slot.message).toEqual({ args: ['hello', [], []], state: 'failed' });

    act(() => result.current.slot.retry());
    expect(send).toHaveBeenCalledTimes(2);
    expect(send).toHaveBeenLastCalledWith('hello', [], []);
    await act(() => Promise.resolve(answer(true)));
    expect(result.current.slot.message).toBeNull();
  });
});
