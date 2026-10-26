import { describe, expect, it } from 'vitest';

import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

import { PROCESS_LINES } from './processEvents';
import { activeView, earlierViews, initialTurnsState, latestPlan, MAX_UNCLAIMED_TURNS, replayView, turnReducer, type TurnAction, type TurnsState } from './turnReducer';

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
const plan = (seq: number, title: string | null): AgentEvent => ({
  turn_id: T,
  seq,
  kind: 'plan_updated',
  payload: {
    plan: title === null ? null : { title, status: 'in_progress', steps: [{ n: 1, step: 'Find the callers', status: 'in_progress' }] },
  },
});
const proposal = (seq: number, id: string, callId = 'p1'): AgentEvent => ({
  turn_id: T,
  seq,
  kind: 'plan_proposed',
  payload: { request_id: id, call_id: callId, plan: '# Rename\n\n1. Find the callers' },
});
const result = (seq: number, id: string): AgentEvent => ({
  turn_id: T,
  seq,
  kind: 'tool_result',
  payload: { call_id: id, ok: true, summary: 'ok', truncated: false },
});

const thinking = (seq: number, value: string): AgentEvent => ({ turn_id: T, seq, kind: 'thinking_delta', payload: { text: value } });
const thought = (seq: number, chars: number): AgentEvent => ({ turn_id: T, seq, kind: 'thinking_end', payload: { chars } });

const retry = (seq: number, textUnits: number, thinkingUnits = 0): AgentEvent => ({
  turn_id: T,
  seq,
  kind: 'model_retry',
  payload: { attempt: 1, max_attempts: 25, code: 'model_gateway.incomplete_stream', message: 'retrying', delay_ms: 1000, text_units: textUnits, thinking_units: thinkingUnits },
});

describe('turnReducer', () => {
  it('drops what a retried model call streamed, and shows the retry until the model streams again', () => {
    // A finished step (thinking, text, a tool), then a call cut off after some reasoning and half an answer.
    const cut = run([thinking(1, 'Plan'), thought(2, 4), text(3, 'First.'), call(4, 'c1'), result(5, 'c1'), thinking(6, 'Hmm'), text(7, 'Half an ansé')]);
    const retried = activeView(run([retry(8, 'Half an ansé'.length, 'Hmm'.length)], cut));
    expect(retried.items.map((i) => i.type)).toEqual(['thinking', 'text', 'tool']);
    expect(retried.retrying).toEqual({ attempt: 1, max_attempts: 25, message: 'retrying' });
    // Never past a tool row, even if the counts say more.
    expect(activeView(run([retry(8, 999, 999)], cut)).items.map((i) => i.type)).toEqual(['thinking', 'text', 'tool']);
    const answered = activeView(run([retry(8, 'Half an ansé'.length, 'Hmm'.length), text(9, 'Whole answer.')], cut));
    expect(answered.retrying).toBeUndefined();
    expect(answered.items[3]).toEqual({ type: 'text', key: 't9', text: 'Whole answer.' });
  });

  it('clears a retry once a sub-agent or the compaction moves on (their calls stream nothing to the turn)', () => {
    const subagent: AgentEvent = { turn_id: T, seq: 2, kind: 'subagent_finished', payload: { agent_id: 'a1', status: 'completed' } as never };
    const meter: AgentEvent = { turn_id: T, seq: 2, kind: 'context', payload: { phase: 'compacted' } as never };
    for (const next of [subagent, meter]) expect(activeView(run([retry(1, 0), next])).retrying).toBeUndefined();
    // Anything else keeps it up.
    expect(activeView(run([retry(1, 0), approval(2, 'r1')])).retrying).toBeDefined();
  });

  it('cuts a retried call\'s text off the end of a text row it extended', () => {
    const view = activeView(run([text(1, 'Kept. '), text(2, 'Lost'), retry(3, 4)]));
    expect(view.items).toEqual([{ type: 'text', key: 't1', text: 'Kept. ' }]);
  });

  it('folds each step\'s reasoning into its own Thinking block, closed by thinking_end', () => {
    const view = activeView(run([thinking(1, 'Look'), thinking(2, 'ing'), thought(3, 7), text(4, 'Answer'), thinking(5, 'Again'), call(6, 'c1')]));
    expect(view.items.map((i) => i.type)).toEqual(['thinking', 'text', 'thinking', 'tool']);
    expect(view.items[0]).toEqual({ type: 'thinking', key: 'r1', text: 'Looking', chars: 7 });
    expect(view.items[2]).toEqual({ type: 'thinking', key: 'r5', text: 'Again' });
  });

  it('keeps thinking and text runs apart when it stores them, and re-folds them the same way', () => {
    const live = activeView(run([thinking(1, 'a'), thinking(2, 'b'), text(3, 'c'), text(4, 'd')]));
    // A late, lower seq re-folds from the stored (merged) runs.
    const refolded = activeView(run([thinking(2, 'b'), text(3, 'c'), text(4, 'd'), thinking(1, 'a')]));
    expect(refolded.items).toEqual(live.items);
    expect(live.items).toEqual([
      { type: 'thinking', key: 'r1', text: 'ab' },
      { type: 'text', key: 't3', text: 'cd' },
    ]);
  });

  it('keeps the model the running status names', () => {
    const model = { name: 'gpt-5', reasoning_effort: 'high' as const, chosen: true, pinned: false };
    const view = activeView(run([{ turn_id: T, seq: 1, kind: 'status', payload: { phase: 'running', model } }, status(2, 'done')]));
    expect(view.model).toEqual(model);
  });

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

  it('matches interleaved results of concurrent calls to their calls by id, not position', () => {
    // Read-only calls of one model step run together (#1213): their results
    // come back in completion order, between the other calls' events.
    const failed: AgentEvent = { turn_id: T, seq: 6, kind: 'tool_result', payload: { call_id: 'c1', ok: false, summary: 'not found', truncated: false } };
    const view = activeView(run([call(1, 'c1'), call(2, 'c2'), call(3, 'c3'), result(4, 'c3'), result(5, 'c2'), failed]));
    expect(view.items.map((item) => (item.type === 'tool' ? [item.callId, item.result?.summary] : null))).toEqual([
      ['c1', 'not found'],
      ['c2', 'ok'],
      ['c3', 'ok'],
    ]);
  });

  it('keeps a commit failure after done until a retry saves it, and shows a later one again', () => {
    const done: AgentEvent = { turn_id: T, seq: 2, kind: 'done', payload: { committed: false, commit: 'pending', conversation_id: '7', message_ids: [], changed_files: 0 } };
    const failed = (seq: number, message: string): AgentEvent => ({ turn_id: T, seq, kind: 'commit_failed', payload: { execution_id: 'e1', code: 'local_turn_unavailable', message } });
    let state = run([status(1, 'done'), done, failed(3, 'not saved')]);
    expect(activeView(state).commitFailed).toMatchObject({ executionId: 'e1', message: 'not saved' });
    state = turnReducer(state, { type: 'commit-saved', turnId: T });
    expect(activeView(state).commitFailed).toBeUndefined();
    // A late re-delivery of the old failure does not bring it back.
    state = run([failed(3, 'not saved'), text(0, 'answer')], state);
    expect(activeView(state).commitFailed).toBeUndefined();
    // A later failure (a retry refused) does.
    state = run([failed(4, 'still not saved')], state);
    expect(activeView(state).commitFailed).toMatchObject({ message: 'still not saved' });
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

  it("drops an MCP sign-in card when its tool call ends, and keeps the others", () => {
    const signIn: AgentEvent = {
      turn_id: T,
      seq: 3,
      kind: 'approval_request',
      payload: { request_id: 's', kind: 'mcp_authorization', call_id: 'c1', tool: 'create_issue', title: 'Jira needs your sign-in', detail: '', reason: '', can_remember: false },
    };
    const view = activeView(run([call(1, 'c1'), approval(2, 'a'), signIn, result(4, 'c1')]));
    expect(view.approvals.map((a) => a.request_id)).toEqual(['a']);
  });

  it('drops a request the host closed at its wait (approval_closed), and keeps the others', () => {
    const closed: AgentEvent = { turn_id: T, seq: 3, kind: 'approval_closed', payload: { request_id: 'a', reason: 'timeout' } };
    const view = activeView(run([approval(1, 'a'), approval(2, 'b'), closed]));
    expect(view.approvals.map((a) => a.request_id)).toEqual(['b']);
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

  it('plan_updated: the latest plan wins, null once deleted, and a replay keeps it', () => {
    const events = [status(0, 'running'), plan(1, 'First'), plan(2, 'Second')];
    expect(activeView(run(events)).plan?.title).toBe('Second');
    expect(replayView(events).plan?.title).toBe('Second');
    expect(activeView(run([...events, plan(3, null)])).plan).toBeNull();
    expect(activeView(run([status(0, 'running')])).plan).toBeUndefined();
  });

  it('latestPlan is the newest turn that sent one, skipping turns that sent none', () => {
    const withPlan = replayView([plan(1, 'Old')]);
    const without = replayView([status(1, 'done')]);
    expect(latestPlan([withPlan, without])?.title).toBe('Old');
    expect(latestPlan([withPlan, replayView([plan(1, null)])])).toBeNull();
    expect(latestPlan([])).toBeNull();
  });

  it('holds a plan proposal until it is answered, and a re-fold does not resurrect it', () => {
    let state = run([status(0, 'running'), proposal(2, 'r1')]);
    expect(activeView(state).planProposal?.request_id).toBe('r1');
    state = turnReducer(state, { type: 'approval-resolved', turnId: T, requestId: 'r1' });
    expect(activeView(state).planProposal).toBeUndefined();
    state = run([text(1, 'early')], state);
    expect(activeView(state).planProposal).toBeUndefined();
  });

  it('drops the proposal once accepted, its call ends, or the turn ends', () => {
    const accepted: AgentEvent = { turn_id: T, seq: 3, kind: 'plan_accepted', payload: { call_id: 'p1' } };
    const view = activeView(run([proposal(2, 'r1'), accepted]));
    expect(view.planProposal).toBeUndefined();
    expect(view.planAccepted).toBe('p1');
    expect(activeView(run([proposal(2, 'r1'), result(3, 'p1')])).planProposal).toBeUndefined();
    for (const phase of ['done', 'cancelled', 'error'] as const) {
      expect(activeView(run([proposal(1, 'r1'), status(2, phase)])).planProposal).toBeUndefined();
    }
    expect(activeView(run([status(1, 'done'), proposal(2, 'late')])).planProposal).toBeUndefined();
    expect(replayView([proposal(1, 'r1')]).planProposal).toBeUndefined();
  });
});

describe('sub-agents (#1226)', () => {
  const spawn = (seq: number, id: string): AgentEvent => ({
    turn_id: T,
    seq,
    kind: 'tool_call',
    payload: { call_id: id, tool: 'spawn_agent', args_summary: 'Find the callers', remote: false },
  });
  const started = (seq: number, agentId: string, parent: string, ordinal: number): AgentEvent => ({
    turn_id: T,
    seq,
    kind: 'subagent_started',
    payload: { agent_id: agentId, parent_call_id: parent, ordinal, label: `child ${ordinal}` },
  });
  const inner = (seq: number, agentId: string, event: Extract<AgentEvent, { kind: 'subagent_event' }>['payload']['inner']): AgentEvent => ({
    turn_id: T,
    seq,
    kind: 'subagent_event',
    payload: { agent_id: agentId, inner: event },
  });
  const finished = (seq: number, agentId: string, status: 'completed' | 'cancelled', result?: string): AgentEvent => ({
    turn_id: T,
    seq,
    kind: 'subagent_finished',
    payload: { agent_id: agentId, status, steps: 2, input_tokens: 100, output_tokens: 20, ...(result === undefined ? {} : { result }) },
  });

  it('opens a lane per child under its spawn call, with its own rows, and settles it', () => {
    const view = activeView(
      run([
        spawn(1, 'c1'),
        started(2, 'a_1', 'c1', 1),
        spawn(3, 'c2'),
        started(4, 'a_2', 'c2', 2),
        inner(5, 'a_1', { kind: 'tool_call', call_id: 'k1', tool: 'read_file', args_summary: 'a.rs', remote: false }),
        inner(6, 'a_1', { kind: 'tool_result', call_id: 'k1', ok: true, summary: 'fn main', truncated: false }),
        inner(7, 'a_1', { kind: 'text_delta', text: 'Found it.' }),
        finished(8, 'a_1', 'completed', 'main is in a.rs'),
      ]),
    );
    // The children's calls are not the turn's own rows.
    expect(view.items.map((item) => (item.type === 'tool' ? item.tool : item.type))).toEqual(['spawn_agent', 'spawn_agent']);
    const [first, second] = view.subAgents ?? [];
    expect(first).toMatchObject({ agentId: 'a_1', parentCallId: 'c1', status: 'completed', steps: 2, inputTokens: 100, result: 'main is in a.rs' });
    expect(first?.items).toEqual([
      { type: 'tool', key: 'ck1', callId: 'k1', tool: 'read_file', argsSummary: 'a.rs', remote: false, result: { ok: true, summary: 'fn main', truncated: false } },
      { type: 'text', key: 't7', text: 'Found it.' },
    ]);
    expect(second).toMatchObject({ agentId: 'a_2', parentCallId: 'c2', status: 'running', label: 'child 2' });
  });

  it('runs again when a continued child sends events, and replays from the history as it ran', () => {
    const events = [spawn(1, 'c1'), started(2, 'a_1', 'c1', 1), finished(3, 'a_1', 'completed', 'one'), inner(4, 'a_1', { kind: 'text_delta', text: 'more' })];
    expect(activeView(run(events)).subAgents?.[0]?.status).toBe('running');
    expect(replayView([...events, finished(5, 'a_1', 'cancelled')]).subAgents?.[0]).toMatchObject({ status: 'cancelled', steps: 2 });
    expect(replayView([...events, finished(5, 'a_1', 'cancelled')]).subAgents?.[0]?.result).toBeUndefined();
  });

  it('ignores an event for a child whose start it never heard, and a repeated start', () => {
    const view = activeView(run([inner(1, 'a_9', { kind: 'text_delta', text: 'x' }), finished(2, 'a_9', 'completed'), spawn(3, 'c1'), started(4, 'a_1', 'c1', 1), started(5, 'a_1', 'c1', 1)]));
    expect(view.subAgents?.map((lane) => lane.agentId)).toEqual(['a_1']);
  });
});

describe('usage, usage_total and context (#1227)', () => {
  const meter = { used_tokens: 1_200, window_tokens: 10_000, percent: 12, phase: 'measured' as const, compacted: false, window_fallback: false };
  const usage = (seq: number, prompt: number, cached?: number): AgentEvent => ({
    turn_id: T,
    seq,
    kind: 'usage',
    payload: { model: 'gpt-x', step: seq, prompt_tokens: prompt, completion_tokens: 10, total_tokens: prompt + 10, reasoning_tokens: 2, ...(cached === undefined ? {} : { cached_tokens: cached }), context: meter },
  });
  const total = (seq: number): AgentEvent => ({
    turn_id: T,
    seq,
    kind: 'usage_total',
    payload: { prompt: 3_500, completion: 30, cached: 900, cache_write: 0, reasoning: 4, calls: 3, subagents: { prompt: 300, completion: 10, cached: 0, cache_write: 0, reasoning: 0, calls: 1 } },
  });

  it('sums the turn’s calls while it runs, with the model and the meter', () => {
    const view = activeView(run([status(0, 'running'), usage(1, 1_000), usage(2, 2_200, 900)]));
    expect(view.usage).toEqual({ prompt: 3_200, completion: 20, cached: 900, cache_write: 0, reasoning: 4, calls: 2, final: false, model: 'gpt-x' });
    expect(view.context?.percent).toBe(12);
  });

  it('takes the host’s total once the run ended, sub-agents included, and keeps it', () => {
    const view = replayView([status(0, 'running'), usage(1, 1_000), usage(2, 2_200, 900), total(3), usage(4, 50)]);
    expect(view.usage).toMatchObject({ prompt: 3_500, cached: 900, calls: 3, final: true, model: 'gpt-x' });
    expect(view.usage?.subagents?.prompt).toBe(300);
  });

  it('follows the context event between usage events', () => {
    const view = activeView(run([usage(1, 1_000), { turn_id: T, seq: 2, kind: 'context', payload: { ...meter, phase: 'compacting' } }]));
    expect(view.context?.phase).toBe('compacting');
    expect(view.usage?.calls).toBe(1);
  });
});

describe('background processes (#1223)', () => {
  const delta = (seq: number, data: string, offset: number, callId = 'c1'): AgentEvent => ({
    turn_id: T,
    seq,
    kind: 'tool_output_delta',
    payload: { call_id: callId, process_id: 'p_0000abcd', stream: 'pty', data, offset },
  });
  const started: AgentEvent = {
    turn_id: T,
    seq: 1,
    kind: 'process_started',
    payload: { process_id: 'p_0000abcd', call_id: 'c1', command: 'npm run dev', tty: true, sandbox: 'workspace-write', loopback: true },
  };
  const toolOf = (state: TurnsState) => activeView(state).items.find((item) => item.type === 'tool');

  it('folds tool_output_delta into its run_command row, escape codes dropped, then settles it', () => {
    const state = run([
      call(0, 'c1'),
      started,
      delta(2, '\u001b[32mready\u001b[0m on :5173\r\n', 0),
      delta(3, 'GET /\r\n', 25),
      { turn_id: T, seq: 4, kind: 'process_exited', payload: { process_id: 'p_0000abcd', call_id: 'c1', status: 'exited', exit_code: 0 } },
    ]);
    const tool = toolOf(state);
    expect(tool?.type === 'tool' && tool.process).toEqual({ processId: 'p_0000abcd', status: 'exited', exitCode: 0, output: 'ready on :5173\nGET /\n', end: 32 });
    // Nothing for a call the turn never showed.
    expect(activeView(run([delta(0, 'x', 0, 'nope')])).items).toEqual([]);
  });

  it('keeps the last 200 lines, in offset order when deltas arrive out of order', () => {
    const lines = Array.from({ length: 250 }, (_, i) => `line ${i}`).join('\n');
    const state = run([call(0, 'c1'), delta(3, `${lines}\n`, 6), delta(2, 'first\n', 0)]);
    const tool = toolOf(state);
    const output = tool?.type === 'tool' ? (tool.process?.output ?? '') : '';
    expect(output.split('\n')).toHaveLength(PROCESS_LINES);
    expect(output.endsWith('line 249\n')).toBe(true);
    // Folded again in seq order: "first" came before the 250 lines, so it is past the kept 200.
    expect(output).not.toContain('first');
    expect(activeView(run([call(0, 'c1'), delta(2, 'b\n', 2), delta(1, 'a\n', 0)])).items[0]).toMatchObject({ process: { output: 'a\nb\n' } });
  });
});
