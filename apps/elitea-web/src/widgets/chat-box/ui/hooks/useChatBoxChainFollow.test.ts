import { act, renderHook } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';

import {
  CHAIN_POLL_MS,
  CHAIN_QUIET_MS,
  mergeChainSnapshot,
  useChatBoxChainFollow,
  type ChainSnapshotReader,
} from './useChatBoxChainFollow';

const AT = '2026-11-03T00:00:00.000Z';
const question: ChatMessage = { id: 'q1', role: 'user', name: 'Alice', content: 'fix it', createdAt: AT };
const answer: ChatMessage = { id: 'a1', role: 'assistant', name: 'Agent', content: 'done', createdAt: AT, questionId: 'q1' };
const pending: NonNullable<ChatMessage['runBrief']> = { state: 'pending' };
const ready: NonNullable<ChatMessage['runBrief']> = { state: 'ready', brief: { goal: 'Fix it', done_when: [{ id: 'c1', text: 'tests pass' }], constraints: [], assumptions: [], ambiguity: 'low' } };
const met: NonNullable<ChatMessage['goalCheck']> = { outcome: 'met', conditions: [], met: 1, total: 1 };

interface Props {
  readonly conversationUuid: string | undefined;
  readonly isStreaming: boolean;
  readonly granted: boolean;
}

function setup(initial: readonly ChatMessage[], read: ChainSnapshotReader, props: Partial<Props> = {}) {
  return renderHook((p: Props) => {
    const [messages, setChatHistory] = useState<readonly ChatMessage[]>(initial);
    const follow = useChatBoxChainFollow({
      enabled: true, projectId: 7, conversationUuid: p.conversationUuid, messages, isStreaming: p.isStreaming,
      setChatHistory, granted: () => p.granted, readSnapshot: read,
    });
    return { messages, setChatHistory, follow };
  }, { initialProps: { conversationUuid: 'conv-1', isStreaming: false, granted: true, ...props } });
}

const advance = async (ms: number): Promise<void> => {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
};

describe('useChatBoxChainFollow (ADR-0045)', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('merges a brief that goes from pending to ready', async () => {
    let brief = pending;
    const read = vi.fn<ChainSnapshotReader>(() => Promise.resolve([question, { ...answer, runBrief: brief }]));
    const hook = setup([question, { ...answer, runBrief: pending }], read);

    await advance(CHAIN_POLL_MS);
    expect(read).toHaveBeenCalledTimes(1);
    expect(hook.result.current.messages[1]?.runBrief).toEqual(pending);

    brief = ready;
    await advance(CHAIN_POLL_MS);
    expect(hook.result.current.messages[1]?.runBrief).toEqual(ready);
    expect(hook.result.current.messages[1]?.content).toBe('done');
  });

  it('stops polling when the goal check arrives', async () => {
    let check: ChatMessage['goalCheck'] | undefined;
    const read = vi.fn<ChainSnapshotReader>(() => Promise.resolve([question, { ...answer, runBrief: ready, ...(check ? { goalCheck: check } : {}) }]));
    const hook = setup([question, { ...answer, runBrief: pending }], read);

    await advance(CHAIN_POLL_MS * 2);
    check = met;
    await advance(CHAIN_POLL_MS);
    expect(hook.result.current.messages[1]?.goalCheck).toEqual(met);
    const calls = read.mock.calls.length;
    await advance(CHAIN_POLL_MS * 10);
    expect(read).toHaveBeenCalledTimes(calls);
  });

  it('appends a continuation Main started after the answer settled in this view', async () => {
    const next: ChatMessage = { id: 'q2', role: 'user', name: 'Alice', content: 'Continue', createdAt: AT, continuedAutomatically: true, continuationIndex: 1 };
    const nextAnswer: ChatMessage = { id: 'a2', role: 'assistant', name: 'Agent', content: 'more', createdAt: AT, questionId: 'q2' };
    let rows: readonly ChatMessage[] = [question, { ...answer, stepLimitReached: true }];
    const read = vi.fn<ChainSnapshotReader>(() => Promise.resolve(rows));
    const hook = setup([question, { ...answer, isStreaming: true }], read, { isStreaming: true });

    // The answer settles (the live final frame shows the step limit).
    act(() => hook.result.current.setChatHistory([question, { ...answer, responseMetadata: { step_limit_reached: true } }]));
    hook.rerender({ conversationUuid: 'conv-1', isStreaming: false, granted: true });

    rows = [question, { ...answer, stepLimitReached: true }, next];
    await advance(CHAIN_POLL_MS);
    expect(hook.result.current.messages.map((m) => m.id)).toEqual(['q1', 'a1', 'q2']);
    expect(hook.result.current.messages[1]?.stepLimitReached).toBe(true);

    rows = [...rows, nextAnswer];
    await advance(CHAIN_POLL_MS);
    expect(hook.result.current.messages.map((m) => m.id)).toEqual(['q1', 'a1', 'q2', 'a2']);
  });

  it('aborts the poll when the person switches conversation', async () => {
    const signals: AbortSignal[] = [];
    const read = vi.fn<ChainSnapshotReader>((signal) => {
      signals.push(signal);
      return new Promise(() => {});
    });
    const hook = setup([question, { ...answer, runBrief: pending }], read);
    await advance(CHAIN_POLL_MS);
    expect(signals).toHaveLength(1);

    act(() => hook.result.current.setChatHistory([]));
    hook.rerender({ conversationUuid: 'conv-2', isStreaming: false, granted: true });
    expect(signals[0]?.aborted).toBe(true);
    await advance(CHAIN_POLL_MS * 5);
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('stops after 3 minutes without a change', async () => {
    const read = vi.fn<ChainSnapshotReader>(() => Promise.resolve([question, { ...answer, runBrief: pending }]));
    setup([question, { ...answer, runBrief: pending }], read);

    await advance(CHAIN_QUIET_MS);
    const calls = read.mock.calls.length;
    expect(calls).toBeGreaterThan(50);
    await advance(CHAIN_QUIET_MS);
    expect(read).toHaveBeenCalledTimes(calls);
  });

  it('stops 15 s after a settle when no brief or check is expected, and not at all when the person stopped the turn', async () => {
    const read = vi.fn<ChainSnapshotReader>(() => Promise.resolve([question, answer]));
    const hook = setup([question, { ...answer, isStreaming: true }], read, { isStreaming: true });
    act(() => hook.result.current.setChatHistory([question, answer]));
    hook.rerender({ conversationUuid: 'conv-1', isStreaming: false, granted: true });
    await advance(60_000);
    expect(read).toHaveBeenCalledTimes(5);

    const stopped = vi.fn<ChainSnapshotReader>(() => Promise.resolve([question, answer]));
    const other = setup([question, { ...answer, isStreaming: true }], stopped, { isStreaming: true });
    act(() => other.result.current.follow.cancel());
    act(() => other.result.current.setChatHistory([question, answer]));
    other.rerender({ conversationUuid: 'conv-1', isStreaming: false, granted: true });
    await advance(60_000);
    expect(stopped).not.toHaveBeenCalled();
  });
});

describe('mergeChainSnapshot', () => {
  it('keeps the live history when nothing changed and never touches a turn in flight', () => {
    const live = [question, answer];
    expect(mergeChainSnapshot(live, [question, answer])).toBe(live);
    const streaming = [question, { ...answer, isStreaming: true }];
    expect(mergeChainSnapshot(streaming, [question, { ...answer, runBrief: ready }])).toBe(streaming);
  });

  it('appends nothing it cannot anchor to the live history', () => {
    const live = [question, answer];
    expect(mergeChainSnapshot(live, [{ ...question, id: 'x' }])).toBe(live);
  });
});
