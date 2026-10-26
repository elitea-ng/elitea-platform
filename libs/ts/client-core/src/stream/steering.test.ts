import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import {
  STEERING_RETRY_DEFAULT_MS,
  STEERING_RETRY_MAX_MS,
  foldSteeringByAnswer,
  isSteeringCarryOver,
  isSteeringTextValid,
  joinSteeringTexts,
  leftoverSteering,
  mergeSteering,
  mergeSteeringAll,
  parseSteeringList,
  parseSteeringMessage,
  steeringErrorCode,
  steeringRetryAfterMs,
  steeringStateOf,
  steeringWaitSeconds,
  visibleSteering,
  type SteeringItem,
} from './steering';

// The `SteeringMessage` example of services/elitea-main/api/openapi/v2.yaml.
const WIRE = {
  id: '7f0c3c9a-2d4e-4b8f-9a51-0d6c2f1e8b11',
  response_message_id: '3b9d2a40-7c55-4e1a-8f0b-6a2c9e4d1f20',
  author_id: 1,
  content: 'Skip the tests for now and only fix the build.',
  state: 'pending',
  revision: 1,
  created_at: '2026-10-26T10:15:02Z',
  updated_at: '2026-10-26T10:15:02Z',
  injected_at: null,
  message_id: null,
};

const item = (patch: Partial<SteeringItem> = {}): SteeringItem => ({ ...(parseSteeringMessage(WIRE) as SteeringItem), ...patch });

describe('parseSteeringMessage', () => {
  it('reads the contract example', () => {
    assert.deepEqual(parseSteeringMessage(WIRE), {
      id: WIRE.id,
      responseMessageId: WIRE.response_message_id,
      authorId: 1,
      content: WIRE.content,
      state: 'pending',
      revision: 1,
      createdAt: WIRE.created_at,
      messageId: null,
    });
  });

  it('refuses a value without id, answer or content', () => {
    assert.equal(parseSteeringMessage(null), undefined);
    assert.equal(parseSteeringMessage({ ...WIRE, id: '' }), undefined);
    assert.equal(parseSteeringMessage({ ...WIRE, response_message_id: undefined }), undefined);
    assert.equal(parseSteeringMessage({ ...WIRE, content: 3 }), undefined);
  });

  it('treats an unknown state as pending', () => {
    assert.equal(steeringStateOf('paused_forever'), 'pending');
    assert.equal(steeringStateOf('turn_ended'), 'turn_ended');
    assert.equal(steeringStateOf('handed_off'), 'handed_off');
    assert.equal(parseSteeringMessage({ ...WIRE, state: 'later' })?.state, 'pending');
  });

  it('reads a list and skips bad rows', () => {
    assert.equal(parseSteeringList({ items: [WIRE, { id: 'x' }] }).length, 1);
    assert.deepEqual(parseSteeringList({}), []);
  });

  it('reads the error code of a refusal', () => {
    assert.equal(steeringErrorCode({ error: 'turn_not_running' }), 'turn_not_running');
    assert.equal(steeringErrorCode('nope'), undefined);
  });
});

describe('mergeSteering', () => {
  it('adds a new item at the end', () => {
    const list = mergeSteering([item()], item({ id: 'b' }));
    assert.deepEqual(list.map((i) => i.id), [WIRE.id, 'b']);
  });

  it('a higher revision replaces, a lower one does not', () => {
    const edited = mergeSteering([item()], item({ revision: 2, content: 'new' }));
    assert.equal(edited[0]?.content, 'new');
    assert.equal(mergeSteering(edited, item({ revision: 1, content: 'old' }))[0]?.content, 'new');
  });

  it('a replayed pending copy does not undo an injection', () => {
    const injected = mergeSteering([item()], item({ state: 'injected', messageId: 'm1' }));
    const replayed = mergeSteering(injected, item());
    assert.equal(replayed[0]?.state, 'injected');
    assert.equal(replayed[0]?.messageId, 'm1');
  });

  it('handed_off is final: a replayed turn_ended or pending copy of the same revision does not undo it', () => {
    const handed = mergeSteering([item({ state: 'turn_ended' })], item({ state: 'handed_off' }));
    assert.equal(handed[0]?.state, 'handed_off');
    assert.equal(mergeSteering(handed, item())[0]?.state, 'handed_off');
  });

  it('merges a list in order', () => {
    const list = mergeSteeringAll([], [item(), item({ state: 'injected', messageId: 'm1' })]);
    assert.equal(list.length, 1);
    assert.equal(list[0]?.state, 'injected');
  });
});

describe('carry-over (a newer message came before the hand-off)', () => {
  const OLD = WIRE.response_message_id;
  const NEWER = '9e8d7c6b-5a49-4382-9170-6f5e4d3c2b1a';
  // The server moved the leftover message to the newer answer: the same id, revision + 1.
  const carried = item({ responseMessageId: NEWER, revision: 2, state: 'pending' });

  it('isSteeringCarryOver needs another answer and a higher revision than the known copy', () => {
    assert.equal(isSteeringCarryOver(OLD, carried, 1), true);
    assert.equal(isSteeringCarryOver(OLD, carried), true);
    assert.equal(isSteeringCarryOver(OLD, carried, 2), false);
    assert.equal(isSteeringCarryOver(NEWER, carried, 1), false);
  });

  it('moves the item by id to the newer answer, before the messages of the newer turn', () => {
    const own = item({ id: 'n1', responseMessageId: NEWER, createdAt: '2026-10-26T10:16:00Z' });
    const lists = { [OLD]: [item({ state: 'turn_ended' }), item({ id: 'o2', state: 'injected' })], [NEWER]: [own] };
    const { lists: next, moved } = foldSteeringByAnswer(lists, NEWER, [carried]);
    assert.deepEqual(moved, [{ id: WIRE.id, from: OLD, to: NEWER }]);
    assert.deepEqual(next[OLD]?.map((i) => i.id), ['o2']);
    assert.deepEqual(next[NEWER]?.map((i) => [i.id, i.state, i.revision]), [[WIRE.id, 'pending', 2], ['n1', 'pending', 1]]);
  });

  it('a replayed old copy does not move the item back, and a new id only adds', () => {
    const lists = { [NEWER]: [carried] };
    const back = foldSteeringByAnswer(lists, OLD, [item({ state: 'turn_ended' })]);
    assert.deepEqual(back.moved, []);
    assert.equal(back.lists[OLD], undefined);
    assert.deepEqual(back.lists[NEWER], [carried]);
    const added = foldSteeringByAnswer({}, OLD, [item()]);
    assert.deepEqual(added.moved, []);
    assert.deepEqual(added.lists[OLD]?.map((i) => i.id), [WIRE.id]);
  });
});

describe('visibleSteering', () => {
  it('hides removed and handed_off items and items that the transcript already holds', () => {
    const list = [item({ id: 'a' }), item({ id: 'b', state: 'removed' }), item({ id: 'c', state: 'injected', messageId: 'm1' }), item({ id: 'd', state: 'injected', messageId: 'm2' }), item({ id: 'e', state: 'handed_off' })];
    assert.deepEqual(visibleSteering(list, new Set(['m1'])).map((i) => i.id), ['a', 'd']);
  });
});

describe('leftoverSteering', () => {
  const list = [item({ id: 'a' }), item({ id: 'b', state: 'turn_ended' }), item({ id: 'c', state: 'injected' }), item({ id: 'd' })];
  it('takes only own items that the turn did not read', () => {
    assert.deepEqual(leftoverSteering(list, new Set(['a', 'b', 'c']), true).map((i) => i.id), ['a', 'b']);
  });
  it('keeps pending items while the turn can still read them', () => {
    assert.deepEqual(leftoverSteering(list, new Set(['a', 'b', 'c']), false).map((i) => i.id), ['b']);
  });
  it('never takes a handed_off item: the hand-off already sent it', () => {
    const handed = [item({ id: 'a', state: 'handed_off' }), item({ id: 'b', state: 'turn_ended' })];
    assert.deepEqual(leftoverSteering(handed, new Set(['a', 'b']), true).map((i) => i.id), ['b']);
  });
});

describe('joinSteeringTexts', () => {
  it('joins with a blank line and skips blank texts', () => {
    assert.equal(joinSteeringTexts([{ content: 'one' }, { content: '  ' }, { content: 'two' }]), 'one\n\ntwo');
  });
  it('checks the size limit', () => {
    assert.equal(isSteeringTextValid('ok'), true);
    assert.equal(isSteeringTextValid(' '), false);
    assert.equal(isSteeringTextValid('x'.repeat(16 * 1024 + 1)), false);
  });
});

describe('steeringRetryAfterMs', () => {
  // The 429 `rate_limited` example of services/elitea-main/api/openapi/v2.yaml.
  const RATE_LIMITED = { error: 'rate_limited', message: 'Too many steering messages. Retry after the interval in Retry-After.', retry_after_ms: 2000 };
  it('reads retry_after_ms from the body first', () => {
    assert.equal(steeringRetryAfterMs({ ...RATE_LIMITED, retry_after_ms: 3500 }, 9000), 3500);
  });
  it('uses the Retry-After wait, then the default, when the body has no valid wait', () => {
    assert.equal(steeringRetryAfterMs({ error: 'rate_limited' }, 4000), 4000);
    assert.equal(steeringRetryAfterMs({ ...RATE_LIMITED, retry_after_ms: 0 }), STEERING_RETRY_DEFAULT_MS);
    assert.equal(steeringRetryAfterMs({ ...RATE_LIMITED, retry_after_ms: '5' }), STEERING_RETRY_DEFAULT_MS);
    assert.equal(steeringRetryAfterMs(undefined), STEERING_RETRY_DEFAULT_MS);
  });
  it('rounds up and caps a very long wait', () => {
    assert.equal(steeringRetryAfterMs({ retry_after_ms: 1200.2 }), 1201);
    assert.equal(steeringRetryAfterMs({ retry_after_ms: 10 * 60_000 }), STEERING_RETRY_MAX_MS);
  });
  it('shows at least one whole second', () => {
    assert.equal(steeringWaitSeconds(2000), 2);
    assert.equal(steeringWaitSeconds(2001), 3);
    assert.equal(steeringWaitSeconds(10), 1);
  });
});
