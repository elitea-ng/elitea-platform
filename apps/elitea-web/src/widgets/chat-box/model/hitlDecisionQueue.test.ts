/**
 * The decision queue's boundary: what it persists, what it refuses, and what
 * it ignores when the stored blob is not its own.
 */
import { beforeEach, describe, expect, it } from 'vitest';

import { createStorage } from '@/shared/lib/storage';

import {
  HITL_QUEUE_LIMITS,
  forgetHitlDecisions,
  parseStoredHitlDecisions,
  queueHitlDecision,
  queuedHitlDecisions,
  retainHitlDecisions,
  type HitlQueueScope,
} from './hitlDecisionQueue';

const STORAGE_KEY = 'chat.hitlDecisions';
const scope = (message = 'answer-1', kind: HitlQueueScope['kind'] = 'hitl'): HitlQueueScope => ({
  conversation: 'conv-1',
  message,
  kind,
});

function stored(): unknown {
  return createStorage('session').getJSON<unknown>(STORAGE_KEY);
}

beforeEach(() => {
  window.sessionStorage.clear();
});

describe('what is stored', () => {
  it('persists only the decision fields, inside the el. namespace', () => {
    queueHitlDecision(scope(), { interruptId: 'int-1', toolCallId: 'call-1', action: 'edit', value: 'shorter, please' });

    expect(window.sessionStorage.getItem(`el.${STORAGE_KEY}`)).not.toBeNull();
    expect(stored()).toEqual({
      version: 1,
      responses: [
        {
          conversation: 'conv-1',
          message: 'answer-1',
          kind: 'hitl',
          decisions: [{ interruptId: 'int-1', toolCallId: 'call-1', action: 'edit', value: 'shorter, please' }],
        },
      ],
    });
  });

  it('replaces a re-decided card instead of queueing it twice', () => {
    queueHitlDecision(scope(), { interruptId: 'int-1', action: 'approve' });
    queueHitlDecision(scope(), { interruptId: 'int-1', action: 'reject' });
    expect(queuedHitlDecisions(scope())).toEqual([{ interruptId: 'int-1', action: 'reject' }]);
  });

  it('keeps authorization decisions value-free', () => {
    const refused = queueHitlDecision(scope('answer-1', 'authorization'), { interruptId: 'auth-1', action: 'authorize', value: 'token' });
    expect(refused).toEqual({ queued: false, reason: 'invalid' });
    expect(queueHitlDecision(scope('answer-1', 'authorization'), { interruptId: 'auth-1', action: 'skip' })).toEqual({ queued: true });
  });

  it('forgets one response without touching another', () => {
    queueHitlDecision(scope('a'), { interruptId: 'int-1', action: 'approve' });
    queueHitlDecision(scope('b'), { interruptId: 'int-2', action: 'approve' });
    forgetHitlDecisions(scope('a'));
    expect(queuedHitlDecisions(scope('a'))).toEqual([]);
    expect(queuedHitlDecisions(scope('b'))).toHaveLength(1);
  });

  it('retains only decisions whose card is still pending', () => {
    queueHitlDecision(scope(), { interruptId: 'int-1', action: 'approve' });
    queueHitlDecision(scope(), { interruptId: 'int-gone', action: 'approve' });
    expect(retainHitlDecisions(scope(), new Set(['int-1', 'int-2'])).map((entry) => entry.interruptId)).toEqual(['int-1']);
    expect(queuedHitlDecisions(scope()).map((entry) => entry.interruptId)).toEqual(['int-1']);
  });
});

describe('bounds, checked before writing', () => {
  it(`admits ${HITL_QUEUE_LIMITS.decisionsPerResponse} decisions per response and refuses one more`, () => {
    for (let index = 1; index <= HITL_QUEUE_LIMITS.decisionsPerResponse; index += 1) {
      expect(queueHitlDecision(scope(), { interruptId: `int-${index}`, action: 'approve' })).toEqual({ queued: true });
    }
    expect(queueHitlDecision(scope(), { interruptId: 'int-17', action: 'approve' })).toEqual({ queued: false, reason: 'too-many' });
    expect(queuedHitlDecisions(scope())).toHaveLength(HITL_QUEUE_LIMITS.decisionsPerResponse);
  });

  it('persists a value of exactly the byte limit and keeps a larger one in memory only', () => {
    const atLimit = 'é'.repeat(HITL_QUEUE_LIMITS.valueBytes / 2);
    const overLimit = `${atLimit}x`;
    queueHitlDecision(scope(), { interruptId: 'int-1', action: 'edit', value: atLimit });
    queueHitlDecision(scope(), { interruptId: 'int-2', action: 'edit', value: overLimit });

    const persisted = parseStoredHitlDecisions(stored());
    expect(persisted[0]?.decisions.map((entry) => entry.interruptId)).toEqual(['int-1']);
    // Still decided for this page life: the set submits with it.
    expect(queuedHitlDecisions(scope()).map((entry) => entry.value?.length)).toEqual([atLimit.length, overLimit.length]);
    forgetHitlDecisions(scope());
    expect(queuedHitlDecisions(scope())).toEqual([]);
  });

  it(`keeps at most ${HITL_QUEUE_LIMITS.responses} responses, evicting the oldest`, () => {
    for (let index = 1; index <= HITL_QUEUE_LIMITS.responses + 1; index += 1) {
      queueHitlDecision(scope(`answer-${index}`), { interruptId: 'int-1', action: 'approve' });
    }
    const persisted = parseStoredHitlDecisions(stored());
    expect(persisted).toHaveLength(HITL_QUEUE_LIMITS.responses);
    expect(persisted.map((entry) => entry.message)).not.toContain('answer-1');
  });

  it('refuses an identity past the bound', () => {
    const longId = 'i'.repeat(HITL_QUEUE_LIMITS.identityChars + 1);
    expect(queueHitlDecision(scope(), { interruptId: longId, action: 'approve' })).toEqual({ queued: false, reason: 'invalid' });
  });
});

describe('a stored blob that is not this module’s is absent', () => {
  const valid = {
    version: 1,
    responses: [{ conversation: 'conv-1', message: 'answer-1', kind: 'hitl', decisions: [{ interruptId: 'int-1', action: 'approve' }] }],
  };

  it('reads a valid blob', () => {
    expect(parseStoredHitlDecisions(valid)).toHaveLength(1);
  });

  it.each([
    ['not JSON-shaped', 'nope'],
    ['an unknown top-level key', { ...valid, extra: true }],
    ['a wrong version', { ...valid, version: 2 }],
    ['an unknown decision key', { ...valid, responses: [{ ...valid.responses[0], decisions: [{ interruptId: 'int-1', action: 'approve', tool_args: {} }] }] }],
    ['an unknown action', { ...valid, responses: [{ ...valid.responses[0], decisions: [{ interruptId: 'int-1', action: 'delete' }] }] }],
    ['a non-string value', { ...valid, responses: [{ ...valid.responses[0], decisions: [{ interruptId: 'int-1', action: 'edit', value: 7 }] }] }],
    ['too many responses', { version: 1, responses: Array.from({ length: 9 }, (_, index) => ({ ...valid.responses[0], message: `m-${index}` })) }],
    [
      'too many decisions',
      { ...valid, responses: [{ ...valid.responses[0], decisions: Array.from({ length: 17 }, (_, index) => ({ interruptId: `i-${index}`, action: 'approve' })) }] },
    ],
    ['an over-limit value', { ...valid, responses: [{ ...valid.responses[0], decisions: [{ interruptId: 'int-1', action: 'edit', value: 'x'.repeat(8193) }] }] }],
  ])('ignores %s', (_label, blob) => {
    expect(parseStoredHitlDecisions(blob)).toEqual([]);
  });

  it('reads corrupt storage as an empty queue, without throwing', () => {
    window.sessionStorage.setItem(`el.${STORAGE_KEY}`, '{not json');
    expect(queuedHitlDecisions(scope())).toEqual([]);
    expect(queueHitlDecision(scope(), { interruptId: 'int-1', action: 'approve' })).toEqual({ queued: true });
    expect(queuedHitlDecisions(scope())).toHaveLength(1);
  });
});
