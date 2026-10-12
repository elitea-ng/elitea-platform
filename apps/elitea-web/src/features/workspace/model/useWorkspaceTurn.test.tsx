import { act, renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { AgentEvent, TurnStartRequest } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';

import { useWorkspaceTurn } from './useWorkspaceTurn';

const REQUEST: TurnStartRequest = {
  workspace_id: 'w1',
  project_id: 1,
  conversation_id: 'c1',
  application_id: 2,
  version_id: 3,
  prompt: 'do it',
  plan_mode: false,
  mentions: [],
  skills: [],
};

const ev = (seq: number, e: Omit<AgentEvent, 'turn_id' | 'seq'>): AgentEvent => ({ turn_id: 'turn-1', seq, ...e }) as AgentEvent;

describe('useWorkspaceTurn', () => {
  it('subscribes once, and unsubscribes on unmount', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { unmount } = renderHook(() => useWorkspaceTurn(ipc));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
    unmount();
    expect(ipc.subscriberCount()).toBe(0);
  });

  it('starts a turn, folds its events, and stops being busy when it is done', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result } = renderHook(() => useWorkspaceTurn(ipc));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));

    await act(() => result.current.start(REQUEST));
    expect(ipc.calls.started).toEqual([REQUEST]);
    expect(result.current.turnId).toBe('turn-1');
    expect(result.current.busy).toBe(true);

    act(() => {
      ipc.emit(ev(1, { kind: 'status', payload: { phase: 'running' } }));
      ipc.emit(ev(2, { kind: 'text_delta', payload: { text: 'working' } }));
    });
    expect(result.current.view.items[0]).toMatchObject({ text: 'working' });
    expect(result.current.busy).toBe(true);

    act(() => {
      ipc.emit(ev(3, { kind: 'done', payload: { committed: true, conversation_id: 'c1', message_ids: [], changed_files: 0 } }));
      ipc.emit(ev(4, { kind: 'status', payload: { phase: 'done' } }));
    });
    expect(result.current.busy).toBe(false);
  });

  it('clears the shown turn only once it is over, and keeps the AGENTS.md list of the running status', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result } = renderHook(() => useWorkspaceTurn(ipc));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
    await act(() => result.current.start(REQUEST));
    act(() => {
      ipc.emit(ev(1, { kind: 'status', payload: { phase: 'running', project_instructions: ['AGENTS.md'] } }));
      ipc.emit(ev(2, { kind: 'text_delta', payload: { text: 'working' } }));
      ipc.emit(ev(3, { kind: 'status', payload: { phase: 'committing' } }));
    });
    expect(result.current.view.projectInstructions).toEqual(['AGENTS.md']);
    act(() => result.current.clear());
    expect(result.current.turnId).toBe('turn-1');
    act(() => {
      ipc.emit(ev(4, { kind: 'done', payload: { committed: true, conversation_id: 'c1', message_ids: [], changed_files: 0 } }));
    });
    act(() => result.current.clear());
    expect(result.current.turnId).toBeNull();
    expect(result.current.view.items).toEqual([]);
  });

  it('stays busy after an error event until the host says done (the failed turn is still committing)', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result } = renderHook(() => useWorkspaceTurn(ipc));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
    await act(() => result.current.start(REQUEST));

    act(() => {
      ipc.emit(ev(1, { kind: 'status', payload: { phase: 'running' } }));
      ipc.emit(ev(2, { kind: 'error', payload: { code: 'model_gateway.member_budget_exhausted', message: 'no budget' } }));
      ipc.emit(ev(3, { kind: 'status', payload: { phase: 'committing' } }));
    });
    expect(result.current.view.error?.code).toBe('model_gateway.member_budget_exhausted');
    expect(result.current.busy).toBe(true);

    act(() => ipc.emit(ev(4, { kind: 'status', payload: { phase: 'error', message: 'no budget' } })));
    expect(result.current.busy).toBe(true);

    act(() => ipc.emit(ev(5, { kind: 'done', payload: { committed: true, conversation_id: 'c1', message_ids: [], changed_files: 0 } })));
    expect(result.current.busy).toBe(false);
  });

  it('shows events that were emitted before start() returned', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result } = renderHook(() => useWorkspaceTurn(ipc));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));

    const original = ipc.startTurn.bind(ipc);
    ipc.startTurn = async (request) => {
      const started = await original(request);
      ipc.emit(ev(1, { kind: 'text_delta', payload: { text: 'first' } })); // the host races ahead of the reply
      return started;
    };
    await act(() => result.current.start(REQUEST));
    expect(result.current.view.items[0]).toMatchObject({ text: 'first' });
  });

  it('queues approvals, answers the head one, and tells the host', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result } = renderHook(() => useWorkspaceTurn(ipc));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
    await act(() => result.current.start(REQUEST));

    const request = (id: string) => ({ request_id: id, tool: 't', title: id, detail: '', reason: '', can_remember: false });
    act(() => {
      ipc.emit(ev(1, { kind: 'approval_request', payload: request('a') }));
      ipc.emit(ev(2, { kind: 'approval_request', payload: request('b') }));
    });
    expect(result.current.view.approvals.map((a) => a.request_id)).toEqual(['a', 'b']);

    await act(() => result.current.answer('a', 'allow_once'));
    expect(ipc.calls.approvals).toEqual([{ requestId: 'a', decision: 'allow_once' }]);
    expect(result.current.view.approvals.map((a) => a.request_id)).toEqual(['b']);
  });

  it('cancels the active turn', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result } = renderHook(() => useWorkspaceTurn(ipc));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
    await act(() => result.current.start(REQUEST));
    await act(() => result.current.cancel());
    expect(ipc.calls.cancelled).toEqual(['turn-1']);
  });

  it('says so when the turn is past cancelling, instead of pretending it stopped', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result } = renderHook(() => useWorkspaceTurn(ipc));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
    await act(() => result.current.start(REQUEST));
    ipc.failNext('cancelTurn', 'turn_not_cancellable', 'The agent has already finished this turn; it can no longer be stopped.');
    await act(() => result.current.cancel());
    expect(result.current.startError).toBe('The agent has already finished this turn; it can no longer be stopped.');
  });

  it('re-syncs with the host when the window comes back, and takes a missed done from it', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result } = renderHook(() => useWorkspaceTurn(ipc));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
    await act(() => result.current.start(REQUEST));

    // Still running: a focus changes nothing.
    act(() => {
      window.dispatchEvent(new Event('focus'));
    });
    await waitFor(() => expect(result.current.busy).toBe(true));

    // The host sent done while the event was lost.
    ipc.setTurnStatus('turn-1', { state: 'done', done: { committed: true, conversation_id: 'c1', message_ids: ['q', 'r'], changed_files: 2 } });
    act(() => {
      window.dispatchEvent(new Event('focus'));
    });
    await waitFor(() => expect(result.current.busy).toBe(false));
    expect(result.current.view.done).toEqual({ committed: true, conversationId: 'c1', messageIds: ['q', 'r'], changedFiles: 2 });

    // The real event arriving late changes nothing.
    act(() => ipc.emit(ev(9, { kind: 'done', payload: { committed: true, conversation_id: 'c1', message_ids: ['q', 'r'], changed_files: 2 } })));
    expect(result.current.busy).toBe(false);
  });

  it('a cancel answered turn_unknown ends the turn in the UI instead of wedging it', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result } = renderHook(() => useWorkspaceTurn(ipc));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
    await act(() => result.current.start(REQUEST));
    ipc.failNext('cancelTurn', 'turn_unknown', 'That turn is not known to this app.');
    ipc.failNext('turnStatus', 'turn_unknown', 'That turn is not known to this app.');
    await act(() => result.current.cancel());
    expect(result.current.busy).toBe(false);
    expect(result.current.view.done?.committed).toBe(false);
  });

  it('a cancel refused as past running re-syncs and shows the committed turn', async () => {
    const ipc = createFakeWorkspaceIpc();
    const { result } = renderHook(() => useWorkspaceTurn(ipc));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
    await act(() => result.current.start(REQUEST));
    ipc.setTurnStatus('turn-1', { state: 'done', done: { committed: true, conversation_id: 'c1', message_ids: [], changed_files: 0 } });
    ipc.failNext('cancelTurn', 'turn_not_cancellable', 'finished');
    await act(() => result.current.cancel());
    expect(result.current.busy).toBe(false);
    expect(result.current.view.done?.committed).toBe(true);
  });

  it('surfaces a failed start instead of staying busy', async () => {
    const ipc = createFakeWorkspaceIpc();
    ipc.startTurn = () => Promise.reject(new Error('no agent runtime'));
    const { result } = renderHook(() => useWorkspaceTurn(ipc));
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));
    let started: boolean | undefined;
    await act(async () => {
      started = await result.current.start(REQUEST);
    });
    expect(started).toBe(false);
    expect(result.current.startError).toBe('no agent runtime');
    expect(result.current.busy).toBe(false);
  });
});
