import { describe, expect, it } from 'vitest';

import { fanoutFixture } from '../__tests__/fanoutFixtures';
import { cardToInterrupt, readInterruptCard } from './fanoutFrames';
import { applyInterruptList, buildDecisionRequest, dropLedgerCardFromMessage, holdsLedgerWork, newDecisionRequestId, readInterruptList } from './interruptLedger';
import type { ChatMessage } from './convertMessagesToChatHistory';
import type { NormalizedHitlInterrupt } from './hitlInterrupts';

const RESPONSE = '10000000-0000-4000-8000-000000000043';
const ID = '0b7672bbbf4eb882481748fb8a0ce16c20f873b838e311a64e6442376c7a5a25';

function message(cards: readonly unknown[], extra: Partial<ChatMessage> = {}): ChatMessage {
  return { id: RESPONSE, role: 'assistant', name: 'Pipeline', content: '', createdAt: '2026-10-19T00:00:00Z', hitlInterrupts: cards, ...extra };
}

describe('buildDecisionRequest (fanout-interrupt-decision-request)', () => {
  it.each([
    ['fanout-interrupt-decision-request-v1', { action: 'approve' }],
    ['fanout-interrupt-decision-request-v1.answer', { action: 'answer', value: 'staging, then prod' }],
    ['fanout-interrupt-decision-request-v1.authorize', { action: 'authorize', credentialRef: 'tsr_4f9a1c2b7d3e8f60' }],
  ])('builds the canonical body of %s', (stem, input) => {
    const fixture = fanoutFixture<{ request_id: string }>(stem);
    expect(buildDecisionRequest({ requestId: fixture.request_id, expectedRevision: 1, ...input })).toEqual(fixture);
  });

  it('sends "" for a valueless action and refuses an empty valued one', () => {
    expect(buildDecisionRequest({ requestId: ID, expectedRevision: 1, action: 'reject', value: 'ignored' })?.value).toBe('');
    expect(buildDecisionRequest({ requestId: ID, expectedRevision: 1, action: 'skip' })?.value).toBe('');
    expect(buildDecisionRequest({ requestId: ID, expectedRevision: 1, action: 'answer', value: '' })).toBeUndefined();
    expect(buildDecisionRequest({ requestId: ID, expectedRevision: 1, action: 'continue' })).toBeUndefined();
  });

  it('never sends authorize without a credential reference, nor a reference with any other action', () => {
    expect(buildDecisionRequest({ requestId: ID, expectedRevision: 1, action: 'authorize' })).toBeUndefined();
    expect(buildDecisionRequest({ requestId: ID, expectedRevision: 1, action: 'approve', credentialRef: 'tsr_4f9a1c2b7d3e8f60' })).not.toHaveProperty('credential_ref');
  });

  it('holds the 8192-character value bound at the limit and refuses limit + 1', () => {
    expect(buildDecisionRequest({ requestId: ID, expectedRevision: 1, action: 'edit', value: 'x'.repeat(8192) })).toBeDefined();
    expect(buildDecisionRequest({ requestId: ID, expectedRevision: 1, action: 'edit', value: 'x'.repeat(8193) })).toBeUndefined();
  });

  it('mints a 64-hex request id that is never all zeros', () => {
    let calls = 0;
    const id = newDecisionRequestId((bytes) => { calls += 1; if (calls > 1) bytes.fill(171); return bytes; });
    expect(id).toBe('ab'.repeat(32));
    expect(calls).toBe(2);
    expect(newDecisionRequestId()).toMatch(/^[0-9a-f]{64}$/u);
  });
});

describe('applyInterruptList (fanout-interrupt-list)', () => {
  const list = fanoutFixture('fanout-interrupt-list-v1');
  const listed = readInterruptList(list) ?? [];

  it('reads both states: DECIDED stays hidden, PENDING is answerable at its revision', () => {
    expect(listed.map((card) => [card.interrupt_id, Boolean(card.hidden), card.ledger_revision])).toEqual([
      ['hitl:sensitive:0001', true, 2],
      ['ask:7f3a9c01', false, 1],
    ]);
  });

  it('replaces the ledger cards of exactly one message and leaves continuation cards alone', () => {
    const root = { interrupt_id: 'root-1', resume_strategy: 'single', message: 'root' };
    const stale = cardToInterrupt(readInterruptCard({ ...fanoutFixture<Record<string, unknown>>('fanout-interrupt-card-v1'), interrupt_id: 'gone', interrupt_key: 'c'.repeat(64) })!);
    const history = [message([root, stale])];
    const [after] = applyInterruptList(history, RESPONSE, listed);
    const cards = (after?.hitlInterrupts ?? []) as readonly NormalizedHitlInterrupt[];
    expect(cards.map((card) => card.interrupt_id)).toEqual(['root-1', 'hitl:sensitive:0001', 'ask:7f3a9c01']);
    expect(after?.hitlInterrupt).toMatchObject({ interrupt_id: 'root-1' });
  });

  it('keeps a card the user just answered hidden while the list still reports it pending', () => {
    const pending = listed[1]!;
    const [after] = applyInterruptList([message([{ ...pending, queued: true, hidden: true }])], RESPONSE, listed);
    expect(((after?.hitlInterrupts ?? []) as readonly NormalizedHitlInterrupt[])[1]).toMatchObject({ hidden: true, queued: true });
  });

  it('refuses a list over the 16-card open cap and skips an invalid nested card', () => {
    expect(readInterruptList(fanoutFixture('fanout-interrupt-list-v1.invalid.seventeen-open'))).toBeUndefined();
    expect(readInterruptList(fanoutFixture('fanout-interrupt-list-v1.invalid.invalid-nested-card'))?.map((card) => card.interrupt_id)).toEqual(['hitl:sensitive:0001']);
  });

  it('drops one card by key and knows when a message still holds ledger work', () => {
    const history = [message(listed)];
    const [after] = dropLedgerCardFromMessage(history, RESPONSE, listed[0]!.interrupt_key!);
    expect((after?.hitlInterrupts ?? []).length).toBe(1);
    expect(holdsLedgerWork(after!)).toBe(true);
    expect(holdsLedgerWork(message([]))).toBe(false);
    expect(holdsLedgerWork(message([], { fanoutParked: true }))).toBe(true);
  });
});
