import { describe, expect, it } from 'vitest';

import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

import { initialTurnsState, replayView, turnReducer, type TurnsState } from './turnReducer';

const T = 'turn-1';
const at = (seq: number, e: Omit<AgentEvent, 'turn_id' | 'seq'>): AgentEvent => ({ turn_id: T, seq, ...e }) as AgentEvent;
const queued = (seq: number, id: string, text: string, revision = 1): AgentEvent => at(seq, { kind: 'steer_queued', payload: { steer_id: id, text, revision } });
const injected = (seq: number, id: string, text: string): AgentEvent => at(seq, { kind: 'steer_injected', payload: { steer_id: id, text } });
const dropped = (seq: number, id: string, text: string, reason: 'removed' | 'turn_ended'): AgentEvent => at(seq, { kind: 'steer_dropped', payload: { steer_id: id, text, reason } });
const toolCall = (seq: number): AgentEvent => at(seq, { kind: 'tool_call', payload: { call_id: 'c1', tool: 'read_file', args_summary: 'a', remote: false } });
const toolResult = (seq: number): AgentEvent => at(seq, { kind: 'tool_result', payload: { call_id: 'c1', ok: true, summary: 'ok', truncated: false } });
const text = (seq: number, value: string): AgentEvent => at(seq, { kind: 'text_delta', payload: { text: value } });

const live = (events: AgentEvent[]): TurnsState =>
  events.reduce<TurnsState>((state, event) => turnReducer(state, { type: 'event', event }), turnReducer(initialTurnsState, { type: 'begin', turnId: T }));

describe('steering events', () => {
  it('keeps a queued message pending, with its newest revision', () => {
    const view = replayView([queued(1, 's1', 'draft'), queued(2, 's1', 'final', 2), queued(3, 's2', 'second')]);
    expect(view.pendingSteers).toEqual([
      { steerId: 's1', text: 'final', revision: 2 },
      { steerId: 's2', text: 'second', revision: 1 },
    ]);
    expect(view.items).toEqual([]);
  });

  it('keeps the newest change when an older one arrives late', () => {
    const state = live([queued(1, 's1', 'draft'), queued(3, 's1', 'final', 3), queued(2, 's1', 'middle', 2)]);
    expect(state.turns[T]?.view.pendingSteers).toEqual([{ steerId: 's1', text: 'final', revision: 3 }]);
  });

  it('shows an injected message as the person’s at the point of the turn where the agent got it', () => {
    const events = [queued(1, 's1', 'also tests'), toolCall(2), toolResult(3), injected(4, 's1', 'also tests'), text(5, 'Sure.')];
    const view = replayView(events);
    expect(view.pendingSteers).toEqual([]);
    expect(view.items.map((item) => item.type)).toEqual(['tool', 'steer', 'text']);
    expect(view.items[1]).toEqual({ type: 'steer', key: 'ss1', steerId: 's1', text: 'also tests' });
    // A reloaded thread folds the same recorded events into the same transcript.
    expect(live(events).turns[T]?.view.items).toEqual(view.items);
  });

  it('keeps the texts the turn ended before taking, but not a removed one', () => {
    const view = replayView([queued(1, 's1', 'a'), queued(2, 's2', 'b'), queued(3, 's3', 'c'), dropped(4, 's2', 'b', 'removed'), dropped(5, 's1', 'a', 'turn_ended'), dropped(6, 's3', 'c', 'turn_ended')]);
    expect(view.pendingSteers).toEqual([]);
    expect(view.endedSteers).toEqual([
      { steerId: 's1', text: 'a' },
      { steerId: 's3', text: 'c' },
    ]);
  });
});
