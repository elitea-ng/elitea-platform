/**
 * A17 — the queue store (ELITEA-2864/2865/2868/2869/2873/2874).
 *
 * Each block states the CASE it holds, not the method it calls: the store is
 * the only place FIFO order, the blank-message refusal and the survive-a-reload
 * contract are decidable without a running model.
 */
import { beforeEach, describe, expect, it } from 'vitest';

import { createStorage } from '@/shared/lib/storage';

import {
  DRAFT_CONVERSATION_KEY,
  createQueuedMessagesStore,
  parsePersistedQueues,
  useQueuedMessagesStore,
} from './queuedMessages.store';

const KEY = 'conv-1';

function freshStore() {
  return createQueuedMessagesStore();
}

beforeEach(() => {
  window.sessionStorage.clear();
  useQueuedMessagesStore.resetForTests();
});

describe('queue order', () => {
  it('delivers in the order the reader typed (ELITEA-2865)', () => {
    const store = freshStore();
    const state = store.getState();
    state.enqueue(KEY, 'first interjection');
    state.enqueue(KEY, 'second interjection');
    state.enqueue(KEY, 'third interjection');

    expect(store.getState().queues[KEY]?.map((row) => row.text)).toEqual([
      'first interjection',
      'second interjection',
      'third interjection',
    ]);
    // FIFO, not LIFO: the head is what a settle delivers, and a message sent
    // LATER must never overtake one already waiting.
    expect(store.getState().dequeue(KEY)?.text).toBe('first interjection');
    expect(store.getState().dequeue(KEY)?.text).toBe('second interjection');
    expect(store.getState().dequeue(KEY)?.text).toBe('third interjection');
    expect(store.getState().dequeue(KEY)).toBeUndefined();
  });

  it('keeps five rapid messages, each exactly once (ELITEA-2874)', () => {
    const store = freshStore();
    const texts = ['Rapid test 1', 'Rapid test 2', 'Rapid test 3', 'Rapid test 4', 'Rapid test 5'];
    for (const text of texts) store.getState().enqueue(KEY, text);
    expect(store.getState().queues[KEY]?.map((row) => row.text)).toEqual(texts);
    // Distinct identities, so the list keys — and `remove` — address one row.
    const ids = new Set(store.getState().queues[KEY]?.map((row) => row.id));
    expect(ids.size).toBe(5);
  });

  it('empties the key entirely rather than leaving an empty list behind', () => {
    const store = freshStore();
    store.getState().enqueue(KEY, 'only one');
    store.getState().dequeue(KEY);
    // Not `[]`: the persisted blob would otherwise grow one entry per
    // conversation ever opened, for the life of the session.
    expect(store.getState().queues[KEY]).toBeUndefined();
  });
});

describe('what cannot be queued (ELITEA-2873)', () => {
  it('refuses an empty, whitespace-only or tab-only message', () => {
    const store = freshStore();
    expect(store.getState().enqueue(KEY, '')).toBeUndefined();
    expect(store.getState().enqueue(KEY, '   ')).toBeUndefined();
    expect(store.getState().enqueue(KEY, '\t\n  \t')).toBeUndefined();
    expect(store.getState().queues[KEY]).toBeUndefined();
  });

  it('keeps the text the reader typed byte for byte (ELITEA-2868)', () => {
    const store = freshStore();
    // Padding and newlines are NOT trimmed away: a stopped run turns queued
    // messages into ordinary ones, and the case requires the text to be
    // "exactly as originally typed".
    const typed = '  Change the approach\n  — focus on key points  ';
    store.getState().enqueue(KEY, typed);
    expect(store.getState().queues[KEY]?.[0]?.text).toBe(typed);
  });
});

describe('remove', () => {
  it('drops one waiting message and leaves the rest in order (ELITEA-2869)', () => {
    const store = freshStore();
    store.getState().enqueue(KEY, 'one');
    const second = store.getState().enqueue(KEY, 'two');
    store.getState().enqueue(KEY, 'three');
    store.getState().remove(KEY, second?.id ?? '');
    expect(store.getState().queues[KEY]?.map((row) => row.text)).toEqual(['one', 'three']);
  });

  it('is a no-op for an id that is not in this queue', () => {
    const store = freshStore();
    store.getState().enqueue(KEY, 'one');
    const before = store.getState().queues[KEY];
    store.getState().remove(KEY, 'not-a-row');
    expect(store.getState().queues[KEY]).toBe(before);
  });
});

describe('per-conversation isolation', () => {
  it('never mixes two conversations', () => {
    const store = freshStore();
    store.getState().enqueue('conv-a', 'for A');
    store.getState().enqueue('conv-b', 'for B');
    expect(store.getState().dequeue('conv-a')?.text).toBe('for A');
    expect(store.getState().queues['conv-b']?.map((row) => row.text)).toEqual(['for B']);
  });

  it('hands the draft chat’s queue to the conversation the first send created', () => {
    const store = freshStore();
    store.getState().enqueue(DRAFT_CONVERSATION_KEY, 'typed before the row existed');
    store.getState().markInterjected(DRAFT_CONVERSATION_KEY, 'question-1');
    store.getState().adopt(DRAFT_CONVERSATION_KEY, 'conv-99');
    // Without this hand-off the FIRST interjection of a conversation — the most
    // common one there is — would be stranded under a key nothing reads again.
    expect(store.getState().queues[DRAFT_CONVERSATION_KEY]).toBeUndefined();
    expect(store.getState().queues['conv-99']?.map((row) => row.text)).toEqual(['typed before the row existed']);
    expect(store.getState().interjected['conv-99']).toEqual(['question-1']);
  });
});

describe('interjection marks', () => {
  it('records each delivered question once', () => {
    const store = freshStore();
    store.getState().markInterjected(KEY, 'q1');
    store.getState().markInterjected(KEY, 'q1');
    store.getState().markInterjected(KEY, 'q2');
    expect(store.getState().interjected[KEY]).toEqual(['q1', 'q2']);
  });

  it('ignores an empty id', () => {
    const store = freshStore();
    store.getState().markInterjected(KEY, '');
    expect(store.getState().interjected[KEY]).toBeUndefined();
  });
});

describe('persistence (ELITEA-2870: it survives a reload)', () => {
  it('re-seeds a fresh store from session storage', () => {
    const first = freshStore();
    first.getState().enqueue(KEY, 'still waiting');
    first.getState().markInterjected(KEY, 'question-7');

    // A reload builds a NEW store in a NEW module instance; only what reached
    // storage comes back. Component state would lose both.
    const second = createQueuedMessagesStore();
    expect(second.getState().queues[KEY]?.map((row) => row.text)).toEqual(['still waiting']);
    expect(second.getState().interjected[KEY]).toEqual(['question-7']);
  });

  it('writes under the namespaced prefix the logout sweep clears', () => {
    const store = freshStore();
    store.getState().enqueue(KEY, 'swept on logout');
    // Raw, un-namespaced keys dodge `clearNamespace()` — the leak §5.4 exists
    // to close. Asserted through the wrapper's own key listing.
    expect(createStorage('session').keys()).toContain('chat.queuedMessages');
  });

  it('treats a corrupt blob as absent rather than throwing', () => {
    window.sessionStorage.setItem('el.chat.queuedMessages', '{not json');
    const store = createQueuedMessagesStore();
    expect(store.getState().queues).toEqual({});
  });
});

describe('parsePersistedQueues', () => {
  it('drops rows that are not the shape this store writes', () => {
    const parsed = parsePersistedQueues({
      queues: {
        good: [{ id: 'a', text: 'kept', queuedAt: 5 }, { id: 7, text: 'dropped' }, null, 'nope'],
        empty: [],
        notAnArray: { id: 'x' },
      },
      interjected: { good: ['q1', 42, null], empty: [] },
    });
    expect(parsed.queues['good']).toEqual([{ id: 'a', text: 'kept', queuedAt: 5 }]);
    expect(parsed.queues['empty']).toBeUndefined();
    expect(parsed.queues['notAnArray']).toBeUndefined();
    expect(parsed.interjected['good']).toEqual(['q1']);
  });

  it('answers an empty record for anything that is not an object', () => {
    expect(parsePersistedQueues(null)).toEqual({ queues: {}, interjected: {} });
    expect(parsePersistedQueues('queue')).toEqual({ queues: {}, interjected: {} });
  });
});
