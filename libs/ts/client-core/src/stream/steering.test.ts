import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import {
  isSteeringTextValid,
  joinSteeringTexts,
  leftoverSteering,
  mergeSteering,
  mergeSteeringAll,
  parseSteeringList,
  parseSteeringMessage,
  steeringErrorCode,
  steeringStateOf,
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

  it('merges a list in order', () => {
    const list = mergeSteeringAll([], [item(), item({ state: 'injected', messageId: 'm1' })]);
    assert.equal(list.length, 1);
    assert.equal(list[0]?.state, 'injected');
  });
});

describe('visibleSteering', () => {
  it('hides removed items and items that the transcript already holds', () => {
    const list = [item({ id: 'a' }), item({ id: 'b', state: 'removed' }), item({ id: 'c', state: 'injected', messageId: 'm1' }), item({ id: 'd', state: 'injected', messageId: 'm2' })];
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
