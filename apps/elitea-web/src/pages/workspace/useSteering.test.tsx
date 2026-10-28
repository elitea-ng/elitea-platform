import { act, renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { useWorkspaceTurn } from '@/features/workspace';
import type { AgentEvent, TurnStartRequest, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';

import { nextTurnMessage, useSteering } from './useSteering';

const REQUEST: TurnStartRequest = { workspace_id: 'w1', project_id: 1, conversation_id: 'c1', application_id: 2, version_id: 3, prompt: 'go', plan_mode: false, mentions: [], skills: [] };
const ev = (seq: number, e: Omit<AgentEvent, 'turn_id' | 'seq'>): AgentEvent => ({ turn_id: 'turn-1', seq, ...e }) as AgentEvent;
const DONE = { committed: false, conversation_id: 'c1', message_ids: [], changed_files: 0 };

async function running(ipc: WorkspaceIpc & { subscriberCount(): number }, sendNext: (prompt: string) => Promise<boolean>) {
  const hook = renderHook(() => {
    const turn = useWorkspaceTurn(ipc);
    return { turn, steering: useSteering(ipc, turn, sendNext) };
  });
  await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
  await act(() => hook.result.current.turn.start(REQUEST));
  return hook;
}

describe('useSteering', () => {
  it('joins the parts of the next turn with a blank line, without empty ones', () => {
    expect(nextTurnMessage(['a', ' ', 'b\nc'])).toBe('a\n\nb\nc');
    expect(nextTurnMessage([])).toBe('');
  });

  it('queues a send while the turn runs, and removes it', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result } = await running(ipc, vi.fn(() => Promise.resolve(true)));
    expect(result.current.steering.active).toBe(true);
    let queued = false;
    await act(async () => {
      queued = await result.current.steering.steer('also tests');
    });
    expect(queued).toBe(true);
    expect(ipc.calls.steers).toEqual([{ turnId: 'turn-1', text: 'also tests' }]);
    act(() => ipc.emit(ev(1, { kind: 'steer_queued', payload: { steer_id: 'steer-1', text: 'also tests', revision: 1 } })));
    expect(result.current.steering.pending).toEqual([{ steerId: 'steer-1', text: 'also tests', revision: 1 }]);
    let removed = false;
    await act(async () => {
      removed = await result.current.steering.remove('steer-1');
    });
    expect(removed).toBe(true);
    expect(ipc.calls.steerUpdates).toEqual([{ turnId: 'turn-1', steerId: 'steer-1', text: null }]);
    ipc.failNext('updateSteer', 'steer_not_pending', 'The agent has it already.');
    await act(async () => {
      removed = await result.current.steering.remove('steer-1');
    });
    expect(removed).toBe(false);
    expect(result.current.steering.error).toBe('The agent has it already.');
  });

  it('sends the texts the turn did not take as one next turn when it ends', async () => {
    const ipc = createFakeWorkspaceIpc();
    const sendNext = vi.fn(() => Promise.resolve(true));
    const { result } = await running(ipc, sendNext);
    act(() => {
      ipc.emit(ev(1, { kind: 'steer_queued', payload: { steer_id: 's1', text: 'first', revision: 1 } }));
      ipc.emit(ev(2, { kind: 'steer_queued', payload: { steer_id: 's2', text: 'second', revision: 1 } }));
      ipc.emit(ev(3, { kind: 'steer_dropped', payload: { steer_id: 's1', text: 'first', reason: 'turn_ended' } }));
      ipc.emit(ev(4, { kind: 'steer_dropped', payload: { steer_id: 's2', text: 'second', reason: 'turn_ended' } }));
      ipc.emit(ev(5, { kind: 'status', payload: { phase: 'done' } }));
    });
    expect(sendNext).not.toHaveBeenCalled();
    act(() => ipc.emit(ev(6, { kind: 'done', payload: DONE })));
    await waitFor(() => expect(sendNext).toHaveBeenCalledWith('first\n\nsecond'));
    expect(sendNext).toHaveBeenCalledTimes(1);
    expect(result.current.steering.active).toBe(false);
  });

  it('keeps a text the host refused as turn_not_running for the next turn', async () => {
    const ipc = createFakeWorkspaceIpc();
    const sendNext = vi.fn(() => Promise.resolve(true));
    const { result } = await running(ipc, sendNext);
    ipc.failNext('steerTurn', 'turn_not_running', 'Not running.');
    let queued = false;
    await act(async () => {
      queued = await result.current.steering.steer('late');
    });
    expect(queued).toBe(true);
    expect(result.current.steering.error).toBeNull();
    act(() => ipc.emit(ev(1, { kind: 'done', payload: DONE })));
    await waitFor(() => expect(sendNext).toHaveBeenCalledWith('late'));
  });

  it('Send now takes the message off the queue, stops the turn, and starts the next turn with it (then what the turn did not take)', async () => {
    const ipc = createFakeWorkspaceIpc();
    const sendNext = vi.fn(() => Promise.resolve(true));
    const { result } = await running(ipc, sendNext);
    act(() => {
      ipc.emit(ev(1, { kind: 'steer_queued', payload: { steer_id: 's1', text: 'queued', revision: 1 } }));
      ipc.emit(ev(2, { kind: 'steer_queued', payload: { steer_id: 's2', text: 'do this instead', revision: 1 } }));
    });
    await act(() => result.current.steering.sendNow('s2'));
    expect(ipc.calls.steerUpdates).toEqual([{ turnId: 'turn-1', steerId: 's2', text: null }]);
    expect(ipc.calls.cancelled).toEqual(['turn-1']);
    expect(sendNext).not.toHaveBeenCalled();
    act(() => {
      ipc.emit(ev(3, { kind: 'steer_dropped', payload: { steer_id: 's2', text: 'do this instead', reason: 'removed' } }));
      ipc.emit(ev(4, { kind: 'steer_dropped', payload: { steer_id: 's1', text: 'queued', reason: 'turn_ended' } }));
      ipc.emit(ev(5, { kind: 'status', payload: { phase: 'cancelled' } }));
      ipc.emit(ev(6, { kind: 'done', payload: DONE }));
    });
    await waitFor(() => expect(sendNext).toHaveBeenCalledWith('do this instead\n\nqueued'));
  });

  it('Send now leaves the turn running when the agent has the message already', async () => {
    const ipc = createFakeWorkspaceIpc();
    const sendNext = vi.fn(() => Promise.resolve(true));
    const { result } = await running(ipc, sendNext);
    act(() => ipc.emit(ev(1, { kind: 'steer_queued', payload: { steer_id: 's1', text: 'taken', revision: 1 } })));
    ipc.failNext('updateSteer', 'steer_not_pending', 'The agent has it already.');
    await act(() => result.current.steering.sendNow('s1'));
    expect(ipc.calls.cancelled).toEqual([]);
    expect(result.current.steering.error).toBe('The agent has it already.');
  });

  it('keeps the texts as unsent when the next turn did not start, and sends them again on retry', async () => {
    const ipc = createFakeWorkspaceIpc();
    const sendNext = vi.fn<(prompt: string) => Promise<boolean>>().mockResolvedValue(false);
    const { result } = await running(ipc, sendNext);
    act(() => {
      ipc.emit(ev(1, { kind: 'steer_dropped', payload: { steer_id: 's1', text: 'lost?', reason: 'turn_ended' } }));
      ipc.emit(ev(2, { kind: 'done', payload: DONE }));
    });
    await waitFor(() => expect(result.current.steering.unsent?.text).toBe('lost?'));
    expect(result.current.steering.error).toContain('back in the message box');
    const firstId = result.current.steering.unsent?.id;
    // A retry that fails keeps the same texts (the composer does not get them again).
    let sent = true;
    await act(async () => {
      sent = (await result.current.steering.unsent?.onRetry('lost? edited')) ?? true;
    });
    expect(sent).toBe(false);
    expect(sendNext).toHaveBeenLastCalledWith('lost? edited');
    expect(result.current.steering.unsent?.id).toBe(firstId);
    sendNext.mockResolvedValue(true);
    await act(async () => {
      sent = (await result.current.steering.unsent?.onRetry('lost? edited')) ?? false;
    });
    expect(sent).toBe(true);
    expect(result.current.steering.unsent).toBeNull();
    expect(result.current.steering.error).toBeNull();
    expect(sendNext).toHaveBeenCalledTimes(3);
  });

  it('forgets the unsent texts once a new turn starts', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result } = await running(ipc, vi.fn(() => Promise.resolve(false)));
    act(() => {
      ipc.emit(ev(1, { kind: 'steer_dropped', payload: { steer_id: 's1', text: 'later', reason: 'turn_ended' } }));
      ipc.emit(ev(2, { kind: 'done', payload: DONE }));
    });
    await waitFor(() => expect(result.current.steering.unsent).not.toBeNull());
    await act(() => result.current.turn.start(REQUEST));
    expect(result.current.steering.unsent).toBeNull();
    expect(result.current.steering.error).toBeNull();
  });
});
