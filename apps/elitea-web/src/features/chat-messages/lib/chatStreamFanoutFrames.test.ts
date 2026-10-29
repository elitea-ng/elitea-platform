/**
 * Fan-out frames (contract v1.1 §9) through the real reducer, fed with the
 * canonical contract fixtures Main and the Worker validate.
 */
import { describe, expect, it } from 'vitest';

import { fanoutFixture, fanoutFixtureText } from '../__tests__/fanoutFixtures';
import { buildHitlBuckets } from './approvalBuckets';
import { fanoutMemberActionId } from './chatStreamFanoutFrames';
import { applyChatStreamFrame } from './chatStreamReducer';
import type { ChatMessage } from './convertMessagesToChatHistory';
import type { NormalizedHitlInterrupt } from './hitlInterrupts';

type Card = Record<string, unknown> & { interrupt_id: string; interrupt_key: string };

const RESPONSE = '10000000-0000-4000-8000-000000000043';
const card = fanoutFixture<Card>('fanout-interrupt-card-v1');
const staticCard = fanoutFixture<Card>('fanout-interrupt-card-v1.static-pause');
const authCard = fanoutFixture<Card>('fanout-interrupt-card-v1.delegated-auth');
const mapCard = fanoutFixture<Card>('fanout-interrupt-card-v1.map-ask-user');

function seed(): readonly ChatMessage[] {
  return [{ id: RESPONSE, role: 'assistant', name: 'Pipeline', content: '', createdAt: '2026-10-19T00:00:00Z', isStreaming: true }];
}

function frame(type: string, responseMetadata: Record<string, unknown>): Record<string, unknown> {
  return { type, message_id: RESPONSE, content: null, response_metadata: responseMetadata };
}

function apply(history: readonly ChatMessage[], ...frames: readonly Record<string, unknown>[]): readonly ChatMessage[] {
  return frames.reduce<readonly ChatMessage[]>((state, next) => applyChatStreamFrame(state, next), history);
}

function cards(history: readonly ChatMessage[]): readonly NormalizedHitlInterrupt[] {
  return (history[0]?.hitlInterrupts ?? []) as readonly NormalizedHitlInterrupt[];
}

const pending = (value: Record<string, unknown>) => frame('agent_interrupt_pending', { pause_progress: true, interrupt_card_v1: value });
const resolved = (value: Record<string, unknown>) => frame('agent_hitl_resolved', { interrupt_resolved_v1: value });
const member = (value: Record<string, unknown>) => frame('agent_fanout_member', { fanout_member_status_v1: value });
const progress = (value: Record<string, unknown>) => frame('agent_fanout_progress', { fanout_progress_v1: value });

describe('agent_interrupt_pending', () => {
  it('raises one card from interrupt_card_v1, placed by its hierarchy and keyed by interrupt_key', () => {
    const [only] = cards(apply(seed(), pending(card)));
    expect(only).toMatchObject({
      interrupt_id: card.interrupt_id,
      interrupt_key: card.interrupt_key,
      ledger_kind: 'tool_guard',
      ledger_revision: 1,
      resume_strategy: 'interrupt_ledger',
      guardrail_type: 'sensitive_tool',
      tool_name: 'create_issue',
      tool_args: { project: 'OPS', summary: 'Disk full' },
      parent_agent_call_id: card['parent_agent_call_id'],
      sibling_ordinal: 1,
    });
    expect(only?.child_thread_id).toBe('');
  });

  it('merges a re-announced card by interrupt_id and keeps the local decided state', () => {
    const first = apply(seed(), pending(card));
    const decided = [{ ...first[0], hitlInterrupts: cards(first).map((entry) => ({ ...entry, decided: true, hidden: true })) }] as readonly ChatMessage[];
    const again = cards(apply(decided, pending(card)));
    expect(again).toHaveLength(1);
    expect(again[0]).toMatchObject({ decided: true, hidden: true });
  });

  it('replaces the card when the same interrupt_id pauses again under a new key', () => {
    const first = apply(seed(), pending(card));
    const decided = [{ ...first[0], hitlInterrupts: cards(first).map((entry) => ({ ...entry, decided: true, hidden: true })) }] as readonly ChatMessage[];
    const nextKey = 'a'.repeat(64);
    const again = cards(apply(decided, pending({ ...card, interrupt_key: nextKey })));
    expect(again).toHaveLength(1);
    expect(again[0]?.interrupt_key).toBe(nextKey);
    expect(again[0]?.hidden).toBeUndefined();
  });

  it('keeps distinct cards apart and leaves the run streaming (the frame is progress)', () => {
    const history = apply(seed(), pending(card), pending(mapCard), pending(staticCard), pending(authCard));
    // The static and auth fixtures reuse the tool card's interrupt_id, so they merge onto it.
    expect(cards(history).map((entry) => entry.interrupt_id)).toEqual([card.interrupt_id, mapCard.interrupt_id]);
    expect(history[0]?.isStreaming).toBe(true);
  });

  it('carries each kind: a static pause offers continue, an auth card its https server', () => {
    expect(cards(apply(seed(), pending(staticCard)))[0]).toMatchObject({ ledger_kind: 'hitl_node', available_actions: ['continue'], node_name: 'review' });
    expect(cards(apply(seed(), pending(authCard)))[0]).toMatchObject({ ledger_kind: 'delegated_auth', server_url: 'https://mcp.atlassian.com/v1/sse' });
  });

  it.each([
    'fanout-interrupt-card-v1.invalid.non-ascii-interrupt-id',
    'fanout-interrupt-card-v1.invalid.fanout-member-without-hierarchy',
  ])('raises nothing from %s', (stem) => {
    const history = seed();
    expect(apply(history, pending(fanoutFixture(stem)))).toBe(history);
  });
});

describe('agent_hitl_resolved', () => {
  it('drops exactly the named card and never renders a value, even one a producer leaked', () => {
    const history = apply(seed(), pending(card), pending(mapCard));
    const leaked = fanoutFixture('fanout-interrupt-resolved-v1.invalid.value-leaked');
    const after = apply(history, resolved(leaked));
    expect(cards(after).map((entry) => entry.interrupt_id)).toEqual([mapCard.interrupt_id]);
    expect(JSON.stringify(after)).not.toContain('staging');
  });

  it('drops a cancelled card the same way, and ignores one for another key', () => {
    const history = apply(seed(), pending(card));
    expect(cards(apply(history, resolved(fanoutFixture('fanout-interrupt-resolved-v1.cancelled'))))).toHaveLength(0);
    const other = { ...fanoutFixture('fanout-interrupt-resolved-v1'), interrupt_key: 'b'.repeat(64) };
    expect(cards(apply(history, resolved(other)))).toEqual(cards(history));
  });

  it('never shows a resolved card again when a restoring fan-out re-announces it', () => {
    const answered = apply(seed(), pending(card), resolved(fanoutFixture('fanout-interrupt-resolved-v1')));
    expect(cards(answered)).toHaveLength(0);
    expect(cards(apply(answered, pending(card)))).toHaveLength(0);
    expect(cards(apply(answered, pending(mapCard))).map((entry) => entry.interrupt_id)).toEqual([mapCard.interrupt_id]);
  });
});

describe('agent_fanout_member and agent_fanout_progress', () => {
  const status = fanoutFixture<Record<string, unknown> & { parent_agent_call_id: string }>('fanout-member-status-v1');
  const progressFixture = fanoutFixture<{ members: { call_id: string; text: string }[] }>('fanout-progress-v1');
  const memberFor = (callId: string, ordinal: number, statusValue = 'start', total = 3) => ({
    ...status, status: statusValue, open_interrupts: statusValue === 'paused' ? 1 : 0,
    fanout_v1: { ...(status['fanout_v1'] as object), kind: total > 16 ? 'map' : 'parallel', ordinal, total },
    parent_agent_call_id: callId, parent_agent_path: [{ call_id: callId, name: 'Researcher', sibling_ordinal: ordinal }],
  });

  it('renders a member as one sub-agent action owned by its member tier', () => {
    const history = apply(seed(), member(status));
    const action = history[0]?.toolActions?.[0] as unknown as Record<string, unknown>;
    expect(action).toMatchObject({ id: fanoutMemberActionId(status.parent_agent_call_id), type: 'llm', name: 'Researcher', status: 'processing', parent_agent_call_id: status.parent_agent_call_id });
    expect(apply(history, member(fanoutFixture('fanout-member-status-v1.end'))).at(0)?.toolActions?.[0]).toMatchObject({ status: 'complete' });
    expect(apply(history, member({ ...status, status: 'failed', open_interrupts: 0 })).at(0)?.toolActions?.[0]).toMatchObject({ status: 'error' });
  });

  it('splits one progress frame into the member accordions by call_id, byte-exact and in order', () => {
    const [first, third] = progressFixture.members;
    const history = apply(seed(), member(memberFor(first!.call_id, 1)), member(memberFor(third!.call_id, 3)));
    const after = apply(history, progress(progressFixture), progress({ ...progressFixture, members: [{ ...first, text: 'more' }] }));
    const byId = new Map((after[0]?.toolActions ?? []).map((action) => [(action as unknown as { id: string }).id, (action as unknown as { content: string }).content]));
    expect(byId.get(fanoutMemberActionId(first!.call_id))).toBe('Searching Jira for more');
    // The fixture's raw text (escapes, markup, non-ASCII) survives unchanged.
    expect(byId.get(fanoutMemberActionId(third!.call_id))).toBe(JSON.parse(fanoutFixtureText('fanout-progress-v1')).members[1].text);
    expect(byId.get(fanoutMemberActionId(third!.call_id))).toBe('Found 2 pages\n<b>ok</b> \u2028 café');
  });

  it('drops an entry for a member it never saw, and the whole of a malformed frame', () => {
    const history = apply(seed(), member(memberFor(progressFixture.members[0]!.call_id, 1)));
    const after = apply(history, progress(progressFixture));
    expect((after[0]?.toolActions ?? []).map((action) => (action as unknown as { id: string }).id)).toEqual([fanoutMemberActionId(progressFixture.members[0]!.call_id)]);
    expect(apply(history, progress(fanoutFixture('fanout-progress-v1.invalid.bad-call-id')))).toBe(history);
  });

  it('appends a 64-member Map flush to 64 accordions', () => {
    const flush = fanoutFixture<{ members: { call_id: string; ordinal: number }[] }>('fanout-progress-v1.map-sixty-four-members');
    const history = apply(seed(), ...flush.members.map((entry) => member(memberFor(entry.call_id, entry.ordinal, 'start', 64))));
    const after = apply(history, progress(flush));
    expect((after[0]?.toolActions ?? []).every((action) => (action as unknown as { content: string }).content === 't')).toBe(true);
    expect(after[0]?.toolActions).toHaveLength(64);
  });

  it('labels the 64th Map member card "Summarizer (64)"', () => {
    const callIds = Array.from({ length: 64 }, (_, index) => `fo1_${index.toString(16).padStart(64, '0')}`);
    callIds[63] = mapCard['parent_agent_call_id'] as string;
    const members = callIds.map((callId, index) => member({
      ...memberFor(callId, index + 1), fanout_v1: { activation: '0a1b2c3d4e5f6071', kind: 'map', member: String(index), node: 'per_ticket', ordinal: index + 1, total: 64 },
      parent_agent_name: 'Summarizer', parent_agent_path: [{ call_id: callId, name: 'Summarizer', sibling_ordinal: index + 1 }],
    }));
    const history = apply(seed(), ...members, pending(mapCard));
    const buckets = buildHitlBuckets(cards(history), history[0]?.toolActions ?? []);
    expect(buckets.subAgents.map((bucket) => bucket.label)).toEqual(['Summarizer (64)']);
  });
});

describe('fanout_parked_v1 (reconcile-only)', () => {
  const parked = fanoutFixture<{ pending: { interrupt_id: string; interrupt_key: string }[] }>('fanout-parked-v1');

  it('drops local ledger cards it does not list, parks the run and never creates a card', () => {
    const history = apply(seed(), pending(card), pending(mapCard));
    const after = apply(history, frame('agent_hitl_interrupt', { fanout_parked_v1: parked }));
    expect(cards(after).map((entry) => entry.interrupt_key)).toEqual([mapCard.interrupt_key]);
    expect(after[0]).toMatchObject({ isStreaming: false, fanoutParked: true });
    const empty = apply(seed(), frame('agent_hitl_interrupt', { fanout_parked_v1: parked }));
    expect(cards(empty)).toHaveLength(0);
  });

  it('ignores a malformed parked list', () => {
    const history = apply(seed(), pending(card));
    const after = apply(history, frame('agent_fanout_progress', { fanout_parked_v1: fanoutFixture('fanout-parked-v1.invalid.nothing-pending') }));
    expect(cards(after)).toHaveLength(1);
  });

  it('keeps the legacy parallel aggregate reconcile-only next to ledger cards', () => {
    const history = apply(seed(), pending(card));
    const aggregate = frame('agent_hitl_interrupt', {
      pause_progress: true,
      hitl_interrupt: { schema: 'elitea.graph.parallel-interrupt.v1', cards: [{ interrupt_id: card.interrupt_id }] },
    });
    expect(cards(apply(history, aggregate)).map((entry) => entry.interrupt_key)).toEqual([card.interrupt_key]);
  });
});
