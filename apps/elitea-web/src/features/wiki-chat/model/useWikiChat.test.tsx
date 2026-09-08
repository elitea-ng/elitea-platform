/**
 * The LOOP, which the pure tests cannot reach.
 *
 * reducer.replay.test.ts proves the frames are read correctly and turn.test.ts
 * proves the transitions are right. Neither can see the hook sending a question
 * twice, polling after the run settled, or leaving a thinking block spinning
 * when the invocation never started — the failures that only exist because
 * there is a loop.
 */
import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { isThinkingBlock, type ChatCapability } from './types';
import { useWikiChat, type ChatInvokeInput, type WikiChatOptions } from './useWikiChat';
import type { ChatInvocationPoll } from '../lib/framesFromChatPoll';

/**
 * The capability toggle's last position, and nothing else.
 *
 * The transcript used to be here too. It is the SERVER's now — elitea-main
 * writes both turns of a wiki chat as they happen — so a conversation the
 * hook opens with arrives as `initialMessages` and one it is handed later
 * arrives through `hydrate`.
 */
function memoryStorage(capability: ChatCapability | null = null) {
  let saved = capability;
  return {
    loadCapability: () => saved,
    saveCapability: (next: ChatCapability) => {
      saved = next;
    },
    get savedCapability() {
      return saved;
    },
  };
}

function harness(overrides: Partial<WikiChatOptions> = {}) {
  const invocations: ChatInvokeInput[] = [];
  const polls: ChatInvocationPoll[] = [];
  let sequence = 0;

  const options: WikiChatOptions = {
    invoke: (input) => {
      invocations.push(input);
      return Promise.resolve('invocation-1');
    },
    poll: () => Promise.resolve(polls.shift() ?? { status: 'InProgress' }),
    storage: memoryStorage(),
    newId: () => `id-${(sequence += 1)}`,
    intervalMs: 5,
    ...overrides,
  };
  return { options, invocations, polls };
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
});
afterEach(() => {
  vi.useRealTimers();
});

describe('sending', () => {
  it('opens a turn and invokes the tool the capability names', async () => {
    const { options, invocations } = harness();
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.setMode('research');
    });
    act(() => {
      result.current.send('  Where is the router?  ');
    });

    await waitFor(() => expect(invocations).toHaveLength(1));
    expect(invocations[0]).toMatchObject({
      toolName: 'deep_research',
      // TRIMMED, and it is the trimmed text that is stored too.
      question: 'Where is the router?',
      capability: 'research',
    });
    expect(result.current.state.messages[0]).toMatchObject({ content: 'Where is the router?' });
    expect(result.current.state.isLoading).toBe(true);
  });

  it('refuses an empty question and a second one while the first is running', async () => {
    const { options, invocations } = harness();
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.send('   ');
    });
    expect(invocations).toHaveLength(0);

    act(() => {
      result.current.send('first');
    });
    await waitFor(() => expect(invocations).toHaveLength(1));
    act(() => {
      result.current.send('second');
    });
    // A second in-flight request would produce two invocations racing to write
    // into one thinking block.
    expect(invocations).toHaveLength(1);
  });

  it('does NOT send the question as its own prior context', async () => {
    const { options, invocations } = harness({
      initialMessages: [
        { role: 'user', content: 'earlier' },
        { role: 'assistant', content: 'earlier answer' },
      ],
    });
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.send('the new one');
    });
    await waitFor(() => expect(invocations).toHaveLength(1));
    expect(invocations[0]?.history).toEqual([
      { role: 'user', content: 'earlier' },
      { role: 'assistant', content: 'earlier answer' },
    ]);
  });
});

describe('an invocation that never starts', () => {
  it('removes the thinking block instead of leaving it spinning', async () => {
    const { options } = harness({
      invoke: () => Promise.reject(new Error('Toolkit settings missing llm_model.')),
    });
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.send('anything');
    });

    await waitFor(() => expect(result.current.state.isLoading).toBe(false));
    expect(result.current.state.messages.filter(isThinkingBlock)).toHaveLength(0);
    expect(result.current.state.messages.at(-1)).toMatchObject({
      isError: true,
      content: 'Sorry, I encountered an error: Toolkit settings missing llm_model.',
    });
    expect(result.current.state.error).toBe('Toolkit settings missing llm_model.');
  });

  it('lets the next question through', async () => {
    let fail = true;
    const { options, invocations } = harness({
      invoke: (input) => {
        invocations.push(input);
        if (fail) {
          fail = false;
          return Promise.reject(new Error('broke'));
        }
        return Promise.resolve('invocation-2');
      },
    });
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.send('first');
    });
    await waitFor(() => expect(result.current.state.isLoading).toBe(false));
    act(() => {
      result.current.send('second');
    });
    await waitFor(() => expect(result.current.state.isLoading).toBe(true));
  });
});

describe('polling', () => {
  it('reduces the frames a poll yields and settles the turn', async () => {
    const { options, polls } = harness();
    polls.push(
      {
        status: 'InProgress',
        custom_events: [
          { data: { message: JSON.stringify({ event: 'thinking', data: { id: 't', message: 'reading' } }) } },
        ],
      },
      { status: 'Completed', result: 'The router is in api/router.go.' },
    );
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.send('where?');
    });
    await waitFor(() => expect(result.current.state.messages.find(isThinkingBlock)?.steps).toHaveLength(1));
    await waitFor(() =>
      expect(result.current.state.messages.at(-1)).toMatchObject({
        role: 'assistant',
        content: 'The router is in api/router.go.',
      }),
    );
    expect(result.current.state.isLoading).toBe(false);
    expect(result.current.state.messages.find(isThinkingBlock)?.status).toBe('completed');
  });

  it('STOPS polling once the run is terminal', async () => {
    // `custom_events` is read-once. A poller that keeps running past the end
    // costs a request every interval for as long as the drawer is open, and it
    // is invisible: the state never changes again.
    let calls = 0;
    const { options } = harness({
      poll: () => {
        calls += 1;
        return Promise.resolve({ status: 'Completed', result: 'done' });
      },
    });
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.send('q');
    });
    await waitFor(() => expect(result.current.state.isLoading).toBe(false));
    const settledAt = calls;

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60);
    });
    expect(calls).toBe(settledAt);
  });

  it('grows the answer across polls and replaces it when the turn completes', async () => {
    // The ANSWER TOKEN channel over the LOOP (issue #701). The events are
    // read-once, so each poll carries only what arrived since the last one
    // and the preview only reads right if every poll is accumulated. A
    // reducer test cannot see a poll that was dropped.
    const fragment = (text: string) => ({
      data: { message: JSON.stringify({ event: 'llm_chunk', data: { text } }) },
    });
    // The terminal poll is queued only AFTER the preview has been read. The
    // poller is faster than a `waitFor` tick, so queuing all three up front
    // would let the run settle — and clear the preview — before the
    // assertion ran, which is a test that passes for the wrong reason on a
    // fast machine and fails on a slow one.
    const { options, polls } = harness();
    polls.push(
      { status: 'InProgress', custom_events: [fragment('The router is ')] },
      { status: 'InProgress', custom_events: [fragment('in api/router.go.')] },
    );
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.send('where?');
    });
    await waitFor(() =>
      expect(result.current.state.streamingText).toBe('The router is in api/router.go.'),
    );

    polls.push({ status: 'Completed', result: 'The router is in api/router.go.' });
    await waitFor(() => expect(result.current.state.isLoading).toBe(false));
    // Cleared by the completion: the finished answer replaces the preview,
    // and leaving it would show the same text twice.
    expect(result.current.state.streamingText).toBe('');
    expect(result.current.state.messages.at(-1)).toMatchObject({
      role: 'assistant',
      content: 'The router is in api/router.go.',
    });
  });

  it('leaves the partial answer on screen when the run is stopped', async () => {
    // DWIKI-012's other half. An interrupted stream must keep what arrived —
    // it is the only record of what the run had produced.
    const { options, polls } = harness();
    polls.push({
      status: 'InProgress',
      custom_events: [
        { data: { message: JSON.stringify({ event: 'llm_chunk', data: { text: 'half an ans' } }) } },
      ],
    });
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.send('where?');
    });
    await waitFor(() => expect(result.current.state.streamingText).toBe('half an ans'));

    polls.push({ status: 'Stopped', message: 'stopped by the user' });
    await waitFor(() => expect(result.current.state.isLoading).toBe(false));
    expect(result.current.state.streamingText).toBe('half an ans');
    expect(result.current.state.messages.at(-1)).toMatchObject({ isError: true });
  });

  it('persists the capability the ANSWER was produced with', async () => {
    const storage = memoryStorage();
    const { options, polls } = harness({ storage });
    polls.push({ status: 'Completed', result: 'answered' });
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.setMode('research');
    });
    act(() => {
      result.current.send('q');
    });
    await waitFor(() => expect(storage.savedCapability).toBe('research'));
  });
});

describe('regenerate', () => {
  it('re-asks the last question with the bad answer removed', async () => {
    const { options, invocations } = harness({
      initialMessages: [
        { role: 'user', content: 'why?' },
        { role: 'assistant', content: 'a bad answer' },
      ],
    });
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.regenerate();
    });
    await waitFor(() => expect(invocations).toHaveLength(1));
    expect(invocations[0]?.question).toBe('why?');
    expect(invocations[0]?.history).toEqual([]);
    expect(
      result.current.state.messages.filter((m) => !isThinkingBlock(m) && m.role === 'assistant'),
    ).toHaveLength(0);
  });

  it('does nothing when there is no question to re-ask', () => {
    const { options, invocations } = harness();
    const { result } = renderHook(() => useWikiChat(options));
    act(() => {
      result.current.regenerate();
    });
    expect(invocations).toHaveLength(0);
  });
});

describe('the conversation that outlives the mount', () => {
  it('opens with the transcript it was given and the stored mode', () => {
    const { options } = harness({
      storage: memoryStorage('research'),
      initialMessages: [{ role: 'user', content: 'from last time' }],
    });
    const { result } = renderHook(() => useWikiChat(options));

    expect(result.current.state.messages).toHaveLength(1);
    expect(result.current.state.mode).toBe('research');
  });

  // The drawer hands over the server's copy of the conversation once it has
  // loaded. Without this the hook could only ever show what it was born with.
  it('hydrate replaces the transcript with a loaded one', () => {
    const { options } = harness({ initialMessages: [{ role: 'user', content: 'local' }] });
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.hydrate([
        { role: 'user', content: 'stored question' },
        { role: 'assistant', content: 'stored answer' },
      ]);
    });
    expect(result.current.state.messages).toEqual([
      { role: 'user', content: 'stored question' },
      { role: 'assistant', content: 'stored answer' },
    ]);
  });

  // THE ONE CASE HYDRATION MUST REFUSE. The server writes the answer when the
  // terminal poll is drained, so its copy is behind the screen's for as long
  // as a turn is in flight — hydrating then would delete a live answer.
  it('hydrate is refused while a turn is running', async () => {
    const { options, invocations } = harness();
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.send('a question');
    });
    await waitFor(() => expect(invocations).toHaveLength(1));
    expect(result.current.state.isLoading).toBe(true);

    act(() => {
      result.current.hydrate([{ role: 'user', content: 'the server’s older copy' }]);
    });
    expect(result.current.state.messages.some((m) => !isThinkingBlock(m) && m.content === 'a question')).toBe(true);
    expect(result.current.state.messages.some((m) => !isThinkingBlock(m) && m.content === 'the server’s older copy')).toBe(false);
  });

  it('clear empties the conversation but keeps the mode', () => {
    const { options } = harness({
      storage: memoryStorage('research'),
      initialMessages: [{ role: 'user', content: 'old' }],
    });
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.clear();
    });
    expect(result.current.state.messages).toEqual([]);
    expect(result.current.state.mode).toBe('research');
  });

  it('restoreCapability follows the last answer, not the toggle', () => {
    const { options } = harness({
      storage: memoryStorage('research'),
      initialMessages: [
        { role: 'user', content: 'q' },
        { role: 'assistant', content: 'a', capability: 'ask' },
      ],
    });
    const { result } = renderHook(() => useWikiChat(options));

    act(() => {
      result.current.setMode('research');
    });
    act(() => {
      result.current.restoreCapability();
    });
    expect(result.current.state.mode).toBe('ask');
  });
});
