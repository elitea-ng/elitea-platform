import { beforeEach, describe, expect, it } from 'vitest';

import type { IndexEvent, IndexStatus } from '@/shared/desktop/indexIpc';
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

import { buildActivities, formatElapsed } from '../lib/activityModel';
import { createActivityFeedStore } from '../model/activityFeed.store';

const at = (seq: number) => ({ turn_id: 't1', seq });
const building: IndexStatus = { state: 'building', files: 0, entities: 0, relations: 0, vectors: 0, chunks: 0, last_run: null, changed_files: 0, error: null, policy_off: false, on_disk: true, watching: false };

function facts(store: ReturnType<typeof createActivityFeedStore>) {
  const { turns, indexes, sends, cloud } = store.getState();
  return { turns, indexes, sends, cloud };
}

describe('the activity feed', () => {
  let store: ReturnType<typeof createActivityFeedStore>;
  beforeEach(() => {
    store = createActivityFeedStore();
  });
  const emit = (event: AgentEvent, now = 1000): void => store.getState().applyAgentEvent(event, now);

  it('follows a turn: running, a tool, a step count, then done', () => {
    store.getState().registerTurn('t1', { workspaceId: 'w1', workspaceName: 'app', conversationId: '77' });
    emit({ ...at(1), kind: 'status', payload: { phase: 'running' } });
    expect(buildActivities(facts(store))[0]).toMatchObject({ kind: 'turn', label: 'Working…', workspaceId: 'w1' });
    emit({ ...at(2), kind: 'tool_call', payload: { call_id: 'c1', tool: 'read_file', args_summary: '', remote: false } });
    expect(buildActivities(facts(store))[0]).toMatchObject({ label: 'Running read_file', detail: 'app · 1 step' });
    emit({ ...at(3), kind: 'tool_result', payload: { call_id: 'c1', ok: true, summary: '', truncated: false } });
    emit({ ...at(4), kind: 'done', payload: { committed: true, conversation_id: '77', message_ids: [], changed_files: 0 } });
    expect(buildActivities(facts(store))).toEqual([]);
  });

  it('ranks an approval above the running turn, and says what to approve', () => {
    emit({ ...at(1), kind: 'status', payload: { phase: 'running' } });
    emit({ ...at(2), kind: 'approval_request', payload: { request_id: 'r1', tool: 'run_command', title: 'Run cargo test', detail: '', reason: '', can_remember: true } });
    const [first, second] = buildActivities(facts(store));
    expect(first).toMatchObject({ kind: 'approval', label: 'Approve: run_command', tone: 'attention' });
    expect(second?.kind).toBe('turn');
    // Another tool's result or streamed text says nothing about the approval.
    emit({ ...at(3), kind: 'tool_result', payload: { call_id: 'c', ok: true, summary: '', truncated: false } });
    emit({ ...at(4), kind: 'text_delta', payload: { text: 'hi' } });
    expect(buildActivities(facts(store))[0]?.kind).toBe('approval');
    // Only the answer (approval_respond), or the host closing it at its wait, closes it.
    store.getState().answerApproval('r1');
    expect(buildActivities(facts(store))[0]?.kind).toBe('turn');
    emit({ ...at(5), kind: 'approval_request', payload: { request_id: 'r2', tool: 'create_issue', title: 'Jira needs your sign-in', detail: '', reason: '', can_remember: false } });
    expect(buildActivities(facts(store))[0]?.kind).toBe('approval');
    emit({ ...at(6), kind: 'approval_closed', payload: { request_id: 'r2', reason: 'timeout' } });
    expect(buildActivities(facts(store))[0]?.kind).toBe('turn');
  });

  it('a proposed plan waits like an approval until it is answered or the turn ends', () => {
    emit({ ...at(1), kind: 'status', payload: { phase: 'running' } });
    emit({ ...at(2), kind: 'plan_updated', payload: { plan: null } });
    expect(buildActivities(facts(store))[0]?.kind).toBe('turn');
    emit({ ...at(3), kind: 'plan_proposed', payload: { request_id: 'p1', call_id: 'c1', plan: '# Plan' } });
    expect(buildActivities(facts(store))[0]).toMatchObject({ kind: 'approval', label: 'Approve: plan' });
    store.getState().answerApproval('p1');
    emit({ ...at(4), kind: 'plan_accepted', payload: { call_id: 'c1' } });
    expect(buildActivities(facts(store))[0]?.kind).toBe('turn');
    emit({ ...at(5), kind: 'plan_proposed', payload: { request_id: 'p2', call_id: 'c2', plan: '# Plan' } });
    emit({ ...at(6), kind: 'status', payload: { phase: 'cancelled' } });
    expect(buildActivities(facts(store)).some((activity) => activity.kind === 'approval')).toBe(false);
  });

  it('drops a turn refused at the start (an error and a terminal status, no `done`)', () => {
    emit({ ...at(1), kind: 'status', payload: { phase: 'resolving' } });
    expect(buildActivities(facts(store))).toHaveLength(1);
    emit({ ...at(2), kind: 'error', payload: { code: 'x', message: 'm' } });
    expect(buildActivities(facts(store))).toEqual([]);
    for (const phase of ['error', 'cancelled', 'done'] as const) {
      const turn = { turn_id: `t-${phase}`, seq: 1 };
      emit({ ...turn, kind: 'status', payload: { phase: 'running' } });
      expect(buildActivities(facts(store)).some((a) => a.turnId === `t-${phase}`)).toBe(true);
      emit({ ...turn, seq: 2, kind: 'status', payload: { phase } });
      expect(buildActivities(facts(store)).some((a) => a.turnId === `t-${phase}`)).toBe(false);
    }
  });

  it('keeps a pending commit visible and a failed one as a warning, until the host says it settled', () => {
    emit({ ...at(1), kind: 'done', payload: { committed: false, commit: 'pending', conversation_id: '77', message_ids: [], changed_files: 1 } });
    expect(buildActivities(facts(store))[0]).toMatchObject({ kind: 'commit', label: 'Saving…' });
    emit({ ...at(2), kind: 'commit_failed', payload: { execution_id: 'e', code: 'x', message: 'no' } });
    expect(buildActivities(facts(store))[0]).toMatchObject({ kind: 'commit', label: 'Not saved', tone: 'warn' });
    store.getState().reconcileTurn('t1', { state: 'done', done: { committed: true, commit: 'committed', conversation_id: '77', message_ids: [], changed_files: 1 } });
    expect(buildActivities(facts(store))).toEqual([]);
  });

  it('reconciles a turn whose events were missed: committing, done, or forgotten by the host', () => {
    emit({ ...at(1), kind: 'status', payload: { phase: 'running' } });
    store.getState().reconcileTurn('t1', { state: 'running', done: null });
    expect(buildActivities(facts(store))[0]?.kind).toBe('turn');
    store.getState().reconcileTurn('t1', { state: 'committing', done: null });
    expect(buildActivities(facts(store))[0]?.kind).toBe('commit');
    store.getState().reconcileTurn('t1', null);
    expect(buildActivities(facts(store))).toEqual([]);
  });

  it('shows an index build with n/m, then drops it when it is ready', () => {
    const event = (phase: IndexEvent['phase'], message: string | null, status = building): IndexEvent => ({ workspace_id: 'w1', phase, message, status });
    store.getState().applyIndexEvent(event('listing', null), 'app');
    expect(buildActivities(facts(store))[0]).toMatchObject({ kind: 'index', label: 'Indexing…' });
    store.getState().applyIndexEvent(event('parsing', '[extract] Parsing 120 files'), 'app');
    store.getState().applyIndexEvent(event('building', '[progress] Processed 40 files'), 'app');
    expect(buildActivities(facts(store))[0]).toMatchObject({ label: 'Indexing 40/120', progress: { done: 40, total: 120 }, cancelKey: 'w1' });
    store.getState().applyIndexEvent(event('ready', null, { ...building, state: 'ready', entities: 9 }), 'app');
    expect(buildActivities(facts(store))).toEqual([]);
  });

  it('shows a send to the cloud with its progress until it completes', () => {
    const send = (phase: 'upload' | 'complete', done: number) => store.getState().applyHandoffEvent({ conversation_uuid: 'u1', phase, done, total: 10, message: null });
    send('upload', 3);
    expect(buildActivities(facts(store))[0]).toMatchObject({ kind: 'handoff', progress: { done: 3, total: 10 }, cancelKey: 'u1' });
    send('complete', 10);
    expect(buildActivities(facts(store))).toEqual([]);
  });

  it('ranks turn > commit > send > cloud > index', () => {
    store.getState().applyIndexEvent({ workspace_id: 'w1', phase: 'listing', message: null, status: building }, 'app');
    store.getState().setCloud('u2', { conversationUuid: 'u2', state: 'CLOUD_RUNNING', title: 'app', workspaceId: 'w1', conversationId: '1' });
    store.getState().applyHandoffEvent({ conversation_uuid: 'u1', phase: 'upload', done: 1, total: 2, message: null });
    emit({ ...at(1), kind: 'done', payload: { committed: false, commit: 'pending', conversation_id: '77', message_ids: [], changed_files: 1 } });
    store.getState().applyAgentEvent({ turn_id: 't2', seq: 1, kind: 'status', payload: { phase: 'running' } }, 5);
    expect(buildActivities(facts(store)).map((a) => a.kind)).toEqual(['turn', 'commit', 'handoff', 'cloud', 'index']);
  });
});

describe('token usage', () => {
  it('sums what the calls sent, keeps the latest call as the context in use, and shows it with the turn', () => {
    const store = createActivityFeedStore();
    const at = (seq: number) => ({ turn_id: 't1', seq });
    store.getState().applyAgentEvent({ ...at(1), kind: 'status', payload: { phase: 'running' } }, 1);
    expect(buildActivities(facts(store))[0]?.stats).toBe('');
    store.getState().applyAgentEvent({ ...at(2), kind: 'usage', payload: { prompt_tokens: 3000, completion_tokens: 200, total_tokens: 3200 } }, 2);
    store.getState().applyAgentEvent({ ...at(3), kind: 'usage', payload: { prompt_tokens: 12_400, completion_tokens: 1000, total_tokens: 13_400 } }, 3);
    expect(store.getState().turns['t1']?.usage).toEqual({ inputTokens: 15_400, outputTokens: 1200, contextTokens: 12_400 });
    expect(buildActivities(facts(store))[0]?.stats).toBe('12.4K context · 1.2K out');
  });
});

describe('formatElapsed', () => {
  it('counts m:ss and h:mm:ss', () => {
    expect(formatElapsed(42_000)).toBe('0:42');
    expect(formatElapsed(125_000)).toBe('2:05');
    expect(formatElapsed(3_725_000)).toBe('1:02:05');
    expect(formatElapsed(-5)).toBe('0:00');
  });
});
