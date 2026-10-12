import { describe, expect, it } from 'vitest';

import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

import { activeView, earlierViews, initialTurnsState, MAX_UNCLAIMED_TURNS, replayView, turnReducer, type TurnAction, type TurnsState } from './turnReducer';

const T = 'turn-1';
const run = (events: AgentEvent[], from: TurnsState = turnReducer(initialTurnsState, { type: 'begin', turnId: T })): TurnsState =>
  events.reduce<TurnsState>((state, event) => turnReducer(state, { type: 'event', event }), from);

const text = (seq: number, value: string): AgentEvent => ({ turn_id: T, seq, kind: 'text_delta', payload: { text: value } });
const status = (seq: number, phase: 'running' | 'done' | 'cancelled' | 'error', message?: string): AgentEvent => ({
  turn_id: T,
  seq,
  kind: 'status',
  payload: message === undefined ? { phase } : { phase, message },
});
const approval = (seq: number, id: string): AgentEvent => ({
  turn_id: T,
  seq,
  kind: 'approval_request',
  payload: { request_id: id, tool: 'shell', title: id, detail: 'd', reason: 'r', can_remember: true },
});
const call = (seq: number, id: string): AgentEvent => ({
  turn_id: T,
  seq,
  kind: 'tool_call',
  payload: { call_id: id, tool: 'read_file', args_summary: 'a.txt', remote: false },
});
const result = (seq: number, id: string): AgentEvent => ({
  turn_id: T,
  seq,
  kind: 'tool_result',
  payload: { call_id: id, ok: true, summary: 'ok', truncated: false },
});

describe('turnReducer', () => {
  it('merges consecutive text deltas into one block and keeps tool rows between blocks', () => {
    const view = activeView(run([text(1, 'Hel'), text(2, 'lo'), call(3, 'c1'), text(4, 'bye')]));
    expect(view.items.map((i) => i.type)).toEqual(['text', 'tool', 'text']);
    expect(view.items[0]).toMatchObject({ text: 'Hello' });
  });

  it('ignores a duplicate delivery of the same seq', () => {
    const view = activeView(run([text(1, 'a'), text(2, 'b'), text(2, 'b'), text(1, 'a')]));
    expect(view.items).toEqual([{ type: 'text', key: 't1', text: 'ab' }]);
  });

  it('re-orders an out-of-order event by seq', () => {
    const view = activeView(run([text(3, 'c'), text(1, 'a'), text(2, 'b')]));
    expect(view.items).toEqual([{ type: 'text', key: 't1', text: 'abc' }]);
  });

  it('attaches a tool_result that arrives before its tool_call', () => {
    const view = activeView(run([result(2, 'c1'), call(1, 'c1')]));
    expect(view.items).toHaveLength(1);
    expect(view.items[0]).toMatchObject({ type: 'tool', callId: 'c1', tool: 'read_file', result: { ok: true } });
  });

  it('shows an unmatched tool_result rather than dropping it', () => {
    const view = activeView(run([result(1, 'ghost')]));
    expect(view.items[0]).toMatchObject({ type: 'tool', callId: 'ghost', result: { summary: 'ok' } });
  });

  it('holds events that arrive before the turn id is known, and shows them once it is', () => {
    const early = run([text(1, 'early')], initialTurnsState);
    expect(activeView(early).items).toEqual([]);
    const begun = turnReducer(early, { type: 'begin', turnId: T });
    expect(activeView(begun).items).toEqual([{ type: 'text', key: 't1', text: 'early' }]);
  });

  it('keeps other turns out of the active view', () => {
    const other: AgentEvent = { turn_id: 'turn-2', seq: 1, kind: 'text_delta', payload: { text: 'nope' } };
    expect(activeView(run([other, text(1, 'mine')])).items).toEqual([{ type: 'text', key: 't1', text: 'mine' }]);
  });

  it('queues approvals oldest first, once each', () => {
    const view = activeView(run([approval(1, 'a'), approval(2, 'b'), approval(1, 'a')]));
    expect(view.approvals.map((a) => a.request_id)).toEqual(['a', 'b']);
  });

  it('removes an answered approval, and a re-fold does not resurrect it', () => {
    let state = run([approval(2, 'b'), approval(3, 'c')]);
    const resolve: TurnAction = { type: 'approval-resolved', turnId: T, requestId: 'b' };
    state = turnReducer(state, resolve);
    expect(activeView(state).approvals.map((a) => a.request_id)).toEqual(['c']);
    // A late, lower-seq event forces a re-fold from the sorted list.
    state = run([approval(1, 'a')], state);
    expect(activeView(state).approvals.map((a) => a.request_id)).toEqual(['a', 'c']);
  });

  it('clears pending approvals when the turn ends, however it ends', () => {
    for (const phase of ['done', 'cancelled', 'error'] as const) {
      const view = activeView(run([approval(1, 'a'), status(2, phase)]));
      expect(view.approvals).toEqual([]);
      expect(view.phase).toBe(phase);
    }
  });

  it('does not queue an approval that arrives after the turn ended', () => {
    expect(activeView(run([status(1, 'done'), approval(2, 'late')])).approvals).toEqual([]);
  });

  it('records status text, the error and the done payload', () => {
    const view = activeView(
      run([
        status(1, 'running', 'working'),
        { turn_id: T, seq: 2, kind: 'error', payload: { code: 'x', message: 'bad' } },
        { turn_id: T, seq: 3, kind: 'done', payload: { committed: true, conversation_id: 'c9', message_ids: ['m1'], changed_files: 2 } },
      ]),
    );
    expect(view).toMatchObject({
      phase: 'running',
      message: 'working',
      error: { code: 'x', message: 'bad' },
      done: { committed: true, conversationId: 'c9', messageIds: ['m1'], changedFiles: 2 },
    });
  });

  it('stores a long answer as one merged run, so appends do not grow the event list', () => {
    const deltas = Array.from({ length: 5_000 }, (_, i) => text(i + 1, 'x'));
    let state = run(deltas);
    const record = state.turns[T];
    expect(record?.events).toHaveLength(1);
    expect(record?.events[0]).toMatchObject({ seq: 1, last: 5_000 });
    expect(activeView(state).items).toEqual([{ type: 'text', key: 't1', text: 'x'.repeat(5_000) }]);
    // A redelivery inside the run is a duplicate; a tool row after it starts a new item.
    state = run([text(2_500, 'x'), call(5_001, 'c1'), text(5_002, 'y')], state);
    expect(state.turns[T]?.events).toHaveLength(3);
    expect(activeView(state).items.map((i) => i.type)).toEqual(['text', 'tool', 'text']);
  });

  it('merges a late delta that closes a gap into the runs around it', () => {
    const state = run([text(1, 'a'), text(3, 'c'), text(2, 'b'), text(4, 'd')]);
    expect(state.turns[T]?.events).toHaveLength(1);
    expect(activeView(state).items).toEqual([{ type: 'text', key: 't1', text: 'abcd' }]);
  });

  it('keeps only the few most recently heard turns it did not begin', () => {
    const other = (n: number): AgentEvent => ({ turn_id: `other-${String(n)}`, seq: 1, kind: 'text_delta', payload: { text: 'x' } });
    const state = run([...Array.from({ length: 10 }, (_, n) => other(n)), text(1, 'mine')]);
    const kept = Object.keys(state.turns);
    expect(kept).toContain(T);
    expect(kept.filter((id) => id !== T)).toHaveLength(MAX_UNCLAIMED_TURNS);
    expect(kept).toContain('other-9');
    expect(kept).not.toContain('other-0');
    // A turn whose start has not returned yet is the newest heard: still there to begin.
    const early = run([text(1, 'early'), ...Array.from({ length: 3 }, (_, n) => other(n))], initialTurnsState);
    expect(activeView(turnReducer(early, { type: 'begin', turnId: T })).items).toEqual([{ type: 'text', key: 't1', text: 'early' }]);
  });

  it('reset forgets everything', () => {
    expect(turnReducer(run([text(1, 'a')]), { type: 'reset' })).toEqual(initialTurnsState);
  });

  it('replays a recorded turn exactly as the live stream folded it, with nothing left to answer', () => {
    const events = [status(0, 'running'), text(3, 'Hello'), call(4, 'c1'), approval(5, 'r1'), result(6, 'c1'), text(7, ' there')];
    const live = activeView(run(events));
    const replayed = replayView([...events].reverse());
    expect(replayed.items).toEqual(live.items);
    expect(live.approvals).toHaveLength(1);
    expect(replayed.approvals).toEqual([]);
  });

  it('adopts a running turn from its record and ignores live events the record already holds', () => {
    // The record merged deltas 2..3 into one row numbered 3.
    const adopted = turnReducer(initialTurnsState, { type: 'adopt', turnId: T, events: [status(1, 'running'), text(3, 'Hel lo')] });
    expect(adopted.activeTurnId).toBe(T);
    expect(earlierViews(adopted)).toEqual([]);
    const after = run([text(2, 'lo'), text(3, 'lo'), text(4, '!'), status(5, 'done')], adopted);
    const view = activeView(after);
    expect(view.items).toEqual([{ type: 'text', key: 't3', text: 'Hel lo!' }]);
    expect(view.phase).toBe('done');
  });
});
