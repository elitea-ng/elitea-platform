/**
 * The re-seed collision rule (`./useChatBoxData.seed.ts`).
 *
 * The second case is the one that was measured rather than imagined: CI's
 * `chat-stream-rust` shard 3 lost the FIRST token of an answer — the bubble
 * held `autotest tts arm 262128` against a stored `MOCK: autotest tts arm
 * 262128` — because a re-seed landed between the first and second
 * `agent_llm_chunk` and replaced the streaming message with the persisted row
 * as it stood when the messages query was fetched. The assertion below is that
 * exact shape: a seed that carries the same turn UNFINISHED must not be
 * allowed to discard what the stream has already put on screen.
 */
import { describe, expect, it } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';

import { hasUnsettledTurn, resolveSeededChatHistory } from './useChatBoxData.seed';

const AT = '2026-09-23T20:12:10.073Z';

const question: ChatMessage = {
  id: 'q1',
  role: 'user',
  name: '',
  content: 'autotest tts arm 262128',
  createdAt: AT,
};
const streaming = (content: string): ChatMessage => ({
  id: 'a1',
  role: 'assistant',
  name: 'Elitea',
  content,
  createdAt: AT,
  isStreaming: true,
  isLoading: false,
});
const settled = (content: string): ChatMessage => ({
  id: 'a1',
  role: 'assistant',
  name: 'Elitea',
  content,
  createdAt: AT,
  isStreaming: false,
  isLoading: false,
});

describe('resolveSeededChatHistory', () => {
  it('keeps the streamed tokens when the seed carries the same turn unfinished', () => {
    const live = [question, streaming('MOCK: ')];
    // What the conversation-messages query held at the moment it was fetched:
    // the row exists, it is still streaming, and its text has not landed yet.
    const seed = [question, streaming('')];

    expect(resolveSeededChatHistory(live, seed, false)).toBe(live);
  });

  it('keeps a turn that is still loading, before its first token', () => {
    const live = [question, { ...streaming(''), isLoading: true }];
    expect(resolveSeededChatHistory(live, [question, settled('MOCK: done')], false)).toBe(live);
  });

  it('takes the seed once the turn has settled', () => {
    const live = [question, settled('MOCK: autotest')];
    const seed = [question, settled('MOCK: autotest tts arm 262128 ')];
    expect(resolveSeededChatHistory(live, seed, false)).toBe(seed);
  });

  it('takes the seed when the reader switched conversation, streaming or not', () => {
    const live = [question, streaming('MOCK: ')];
    const seed = [{ ...question, id: 'q2', content: 'another conversation' }];
    expect(resolveSeededChatHistory(live, seed, true)).toBe(seed);
  });

  it('never lets an empty seed erase a live transcript', () => {
    const live = [question, settled('MOCK: answered')];
    expect(resolveSeededChatHistory(live, [], false)).toBe(live);
  });

  it('takes the seed when there is nothing live to protect', () => {
    const seed = [question, settled('MOCK: answered')];
    expect(resolveSeededChatHistory([], seed, false)).toBe(seed);
  });
});

describe('hasUnsettledTurn', () => {
  it('is false for an empty transcript', () => {
    expect(hasUnsettledTurn([])).toBe(false);
  });

  it('reads the LAST message only, so a stale flag further up cannot freeze the surface', () => {
    expect(hasUnsettledTurn([streaming('half a turn ago'), settled('done')])).toBe(false);
  });
});
