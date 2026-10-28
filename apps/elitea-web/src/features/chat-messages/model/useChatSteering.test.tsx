/**
 * Steering during execution (client contract 1.27): where a send goes while a
 * cloud turn runs, the fallbacks of each refusal, and the end-of-turn rule
 * (the hand-off takes the leftover own messages; ONLY the texts that it
 * returns become ONE next message). The bodies come from the v2.yaml examples through the generated
 * MSW mocks; a refusal is a typed `SteeringError` constant.
 */
import { ThemeProvider } from '@mui/material/styles';
import { act, render, renderHook, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';
import {
  getHandOffChatExecutionSteeringMockHandler,
  getHandOffChatExecutionSteeringResponseMock,
  getListChatExecutionSteeringMockHandler,
  getListChatExecutionSteeringResponseMock,
  getRemoveChatExecutionSteeringMockHandler,
  getSteerChatExecutionMockHandler,
  getSteerChatExecutionResponseMock,
} from '@/shared/api/generated/chat/chat.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { SteeringError, SteeringMessage } from '@/shared/api/generated/model';
import { server } from '@/test/setup';

import { SteeringMessages } from '../ui/chat-box/SteeringMessages';
import { useOwnPendingSteering } from './ownPendingSteering';
import { applySteeringEvent, useChatSteeringStore } from './chatSteering.store';
import { useChatSteering, type UseChatSteeringParams } from './useChatSteering';

const BASE = '/api/v2';
const EXAMPLE = getSteerChatExecutionResponseMock();
const RESPONSE = EXAMPLE.response_message_id;
const ROUTE = `${BASE}/elitea_core/task/prompt_lib/7/${RESPONSE}/steering`;
const KEY = 'conv-steer';
const target = { projectId: 7, messageGroupUuid: RESPONSE };
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const refuse = (code: SteeringError['error'], status = 409) =>
  http.post(ROUTE, () => HttpResponse.json({ error: code } satisfies SteeringError, { status }));
const row = (patch: Partial<SteeringMessage>): SteeringMessage => ({ ...EXAMPLE, ...patch });
// The 429 example of v2.yaml. `retry_after_ms` is typed here until the generated `SteeringError` has it.
type RateLimitedBody = SteeringError & { readonly retry_after_ms?: number };
const RATE_LIMITED: RateLimitedBody = { error: 'rate_limited', message: 'Too many steering messages. Retry after the interval in Retry-After.', retry_after_ms: 2000 };

function setup(overrides: Partial<UseChatSteeringParams> = {}) {
  const send = vi.fn();
  const queueAfterTurn = vi.fn();
  const params: UseChatSteeringParams = { conversationKey: KEY, isStreaming: true, streamTarget: () => target, send, queueAfterTurn, ...overrides };
  const hook = renderHook((props: UseChatSteeringParams) => useChatSteering(props), { initialProps: params });
  return { ...hook, send, queueAfterTurn, params };
}

beforeEach(() => {
  sessionStorage.clear();
  useChatSteeringStore.resetForTests();
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('useChatSteering — where a send goes', () => {
  it('steers the running turn with an Idempotency-Key and records the message as this tab’s own', async () => {
    let key: string | null = null;
    server.use(getSteerChatExecutionMockHandler(async ({ request }) => {
      key = request.headers.get('Idempotency-Key');
      expect(await request.json()).toEqual({ content: EXAMPLE.content });
      return EXAMPLE;
    }));
    const { result, send } = setup();
    act(() => { result.current.onSend(EXAMPLE.content); });
    await waitFor(() => { expect(useChatSteeringStore.getState().items[RESPONSE]).toHaveLength(1); });
    expect(key).toMatch(/^[A-Za-z0-9_-]{1,128}$/);
    expect(useChatSteeringStore.getState().own[RESPONSE]).toEqual({ projectId: '7', conversationKey: KEY, ids: [EXAMPLE.id] });
    expect(send).not.toHaveBeenCalled();
  });

  it('sends the old way when no turn runs, or when the transport owns no run', () => {
    const idle = setup({ isStreaming: false });
    act(() => { idle.result.current.onSend('hello'); });
    expect(idle.send).toHaveBeenCalledWith('hello');
    const unowned = setup({ streamTarget: () => null });
    act(() => { unowned.result.current.onSend('hi'); });
    expect(unowned.send).toHaveBeenCalledWith('hi');
  });

  it('steering_not_supported: the text waits for the turn end, and later sends of the turn skip steering', async () => {
    const calls = vi.fn();
    server.use(http.post(ROUTE, () => { calls(); return HttpResponse.json({ error: 'steering_not_supported' } satisfies SteeringError, { status: 409 }); }));
    const { result, send, queueAfterTurn } = setup();
    act(() => { result.current.onSend('first'); });
    await waitFor(() => { expect(queueAfterTurn).toHaveBeenCalledWith('first'); });
    act(() => { result.current.onSend('second'); });
    expect(send).toHaveBeenCalledWith('second');
    expect(calls).toHaveBeenCalledTimes(1);
    expect(result.current.notice).toBeNull();
  });

  it('turn_not_running: the text goes as a normal message after the turn, with no notice', async () => {
    server.use(refuse('turn_not_running'));
    const { result, queueAfterTurn } = setup();
    act(() => { result.current.onSend('late'); });
    await waitFor(() => { expect(queueAfterTurn).toHaveBeenCalledWith('late'); });
    expect(result.current.notice).toBeNull();
  });

  it('steering_queue_full: the text waits for the turn end and a notice says so', async () => {
    server.use(refuse('steering_queue_full'));
    const { result, queueAfterTurn } = setup();
    act(() => { result.current.onSend('one more'); });
    await waitFor(() => { expect(result.current.notice).toMatch(/too many waiting messages/); });
    expect(queueAfterTurn).toHaveBeenCalledWith('one more');
  });
});

describe('useChatSteering — rate_limited (429)', () => {
  beforeEach(() => { vi.useFakeTimers({ shouldAdvanceTime: true }); });
  afterEach(() => { vi.useRealTimers(); });

  /** Answers 429 with `bodies` in order, then the example message (202). Records the key of each POST. */
  function limitedThenOk(bodies: readonly RateLimitedBody[]) {
    const keys: (string | null)[] = [];
    const queue = [...bodies];
    server.use(http.post(ROUTE, ({ request }) => {
      keys.push(request.headers.get('Idempotency-Key'));
      const next = queue.shift();
      return next ? HttpResponse.json(next, { status: 429, headers: { 'Retry-After': '9' } }) : HttpResponse.json(EXAMPLE, { status: 202 });
    }));
    return keys;
  }

  it('counts down retry_after_ms, then sends the same text again with the SAME Idempotency-Key', async () => {
    const keys = limitedThenOk([{ ...RATE_LIMITED, retry_after_ms: 3_000 }]);
    const { result, queueAfterTurn } = setup();
    act(() => { result.current.onSend(EXAMPLE.content); });
    await waitFor(() => { expect(result.current.notice).toMatch(/You can send again in 3 s/); });
    await act(async () => { await vi.advanceTimersByTimeAsync(1_100); });
    expect(result.current.notice).toMatch(/You can send again in 2 s/);
    expect(keys).toHaveLength(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(2_000); });
    await waitFor(() => { expect(useChatSteeringStore.getState().own[RESPONSE]?.ids).toEqual([EXAMPLE.id]); });
    expect(keys).toHaveLength(2);
    expect(keys[1]).toBe(keys[0]);
    expect(queueAfterTurn).not.toHaveBeenCalled();
    expect(result.current.notice).toBeNull();
  });

  it('waits the default 2 s when the body has no retry_after_ms', async () => {
    const keys = limitedThenOk([{ error: 'rate_limited' }]);
    const { result } = setup();
    act(() => { result.current.onSend('hello'); });
    await waitFor(() => { expect(result.current.notice).toMatch(/You can send again in 2 s/); });
    await act(async () => { await vi.advanceTimersByTimeAsync(2_100); });
    await waitFor(() => { expect(keys).toHaveLength(2); });
    expect(keys[1]).toBe(keys[0]);
  });

  it('after the last try, the text waits for the turn end ONE time and a notice says so', async () => {
    const keys = limitedThenOk(Array.from({ length: 10 }, () => ({ ...RATE_LIMITED, retry_after_ms: 100 })));
    const { result, queueAfterTurn } = setup();
    act(() => { result.current.onSend('busy'); });
    await act(async () => { await vi.advanceTimersByTimeAsync(1_000); });
    await waitFor(() => { expect(queueAfterTurn).toHaveBeenCalledTimes(1); });
    expect(queueAfterTurn).toHaveBeenCalledWith('busy');
    expect(keys).toHaveLength(4);
    expect(new Set(keys).size).toBe(1);
    expect(result.current.notice).toMatch(/could not reach the running turn/);
  });
});

describe('useChatSteering — the end of the turn', () => {
  function ownAndList(ids: readonly string[], items: readonly SteeringMessage[]) {
    useChatSteeringStore.getState().addOwn(RESPONSE, { projectId: '7', conversationKey: KEY }, ids[0] ?? '');
    for (const id of ids.slice(1)) useChatSteeringStore.getState().addOwn(RESPONSE, { projectId: '7', conversationKey: KEY }, id);
    server.use(http.get(ROUTE, () => HttpResponse.json({ ...getListChatExecutionSteeringResponseMock(), items })));
  }

  /**
   * Answers the hand-off with `answers` in order (a number is that HTTP status, with a typed
   * `SteeringError`). Records the key and the `question_id` of each call. Only a list answer uses the generated handler.
   */
  function handOff(answers: readonly (readonly SteeringMessage[] | number)[]) {
    const keys: (string | null)[] = [];
    const questions: string[] = [];
    const queue = [...answers];
    const generated = getHandOffChatExecutionSteeringMockHandler(async ({ request }) => {
      keys.push(request.headers.get('Idempotency-Key'));
      questions.push(((await request.json()) as { question_id: string }).question_id);
      const next = queue.shift();
      return { ...getHandOffChatExecutionSteeringResponseMock(), items: Array.isArray(next) ? [...next] : [] };
    });
    server.use(
      http.post(`${ROUTE}/handoff`, async ({ request }) => {
        const next = queue[0];
        if (typeof next !== 'number') return undefined;
        queue.shift();
        keys.push(request.headers.get('Idempotency-Key'));
        questions.push(((await request.clone().json()) as { question_id: string }).question_id);
        return HttpResponse.json({ error: 'steering_invalid' } satisfies SteeringError, { status: next });
      }),
      generated,
    );
    return Object.assign(keys, { questions });
  }
  const handed = (patch: Partial<SteeringMessage>): SteeringMessage => ({ ...getHandOffChatExecutionSteeringResponseMock().items[0] as SteeringMessage, ...patch });

  it('hands off the own turn_ended messages and sends ONLY the texts that the hand-off returns, as ONE message, with no DELETE', async () => {
    const deleted = vi.fn();
    server.use(getRemoveChatExecutionSteeringMockHandler(() => { deleted(); }));
    ownAndList(['a', 'b', 'c'], [
      row({ id: 'a', content: 'first', state: 'turn_ended' }),
      row({ id: 'b', content: 'read', state: 'injected', message_id: 'm-b' }),
      row({ id: 'c', content: 'second', state: 'turn_ended' }),
      row({ id: 'x', content: 'not mine', state: 'turn_ended' }),
    ]);
    // The server took `c` first; the hand-off returns `a` and a message of another tab of the same user.
    const keys = handOff([[handed({ id: 'a', content: 'first' }), handed({ id: 'y', content: 'other tab' })]]);
    const { rerender, params, queueAfterTurn } = setup();
    rerender({ ...params, isStreaming: false });
    await waitFor(() => { expect(useChatSteeringStore.getState().own[RESPONSE]).toBeUndefined(); });
    expect(keys).toHaveLength(1);
    expect(keys[0]).toMatch(/^[A-Za-z0-9_-]{1,128}$/);
    expect(queueAfterTurn).toHaveBeenCalledTimes(1);
    // The send uses the question id that the hand-off named.
    expect(keys.questions[0]).toMatch(/^[0-9a-f-]{36}$/);
    expect(queueAfterTurn).toHaveBeenCalledWith('first\n\nother tab', keys.questions[0]);
    expect(deleted).not.toHaveBeenCalled();
    expect(useChatSteeringStore.getState().items[RESPONSE]?.find((item) => item.id === 'a')?.state).toBe('handed_off');
  });

  it('sends nothing when the hand-off answers an empty list (the server took the messages first)', async () => {
    ownAndList(['a'], [row({ id: 'a', content: 'first', state: 'turn_ended' })]);
    const keys = handOff([[]]);
    const { rerender, params, queueAfterTurn } = setup();
    rerender({ ...params, isStreaming: false });
    await waitFor(() => { expect(useChatSteeringStore.getState().own[RESPONSE]).toBeUndefined(); });
    expect(keys).toHaveLength(1);
    expect(queueAfterTurn).not.toHaveBeenCalled();
  });

  it('a failed hand-off keeps its key (also in sessionStorage), and the retry after a reload sends the SAME key', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      ownAndList(['a'], [row({ id: 'a', content: 'first', state: 'turn_ended' })]);
      const keys = handOff([502, [handed({ id: 'a', content: 'first' })]]);
      const { rerender, params, queueAfterTurn } = setup();
      rerender({ ...params, isStreaming: false });
      await waitFor(() => { expect(keys).toHaveLength(1); });
      await waitFor(() => { expect(sessionStorage.getItem('el.chat.steeringOwn')).toContain(String(keys[0])); });
      expect(sessionStorage.getItem('el.chat.steeringOwn')).toContain(String(keys.questions[0]));
      expect(queueAfterTurn).not.toHaveBeenCalled();
      // A reload: the record and its key come back from sessionStorage.
      useChatSteeringStore.resetForTests();
      expect(useChatSteeringStore.getState().own[RESPONSE]?.handoffKey).toBe(keys[0]);
      await act(async () => { await vi.advanceTimersByTimeAsync(1_600); });
      await waitFor(() => { expect(queueAfterTurn).toHaveBeenCalledWith('first', keys.questions[0]); });
      expect([...keys]).toEqual([keys[0], keys[0]]);
      expect(keys.questions).toEqual([keys.questions[0], keys.questions[0]]);
      expect(queueAfterTurn).toHaveBeenCalledTimes(1);
      expect(useChatSteeringStore.getState().own[RESPONSE]).toBeUndefined();
    } finally {
      vi.useRealTimers();
    }
  });

  it('sends nothing while the server still shows own messages as pending (the turn can still read them)', async () => {
    ownAndList(['a'], [row({ id: 'a', state: 'pending' })]);
    const keys = handOff([]);
    const { rerender, params, queueAfterTurn } = setup();
    rerender({ ...params, isStreaming: false });
    await waitFor(() => { expect(useChatSteeringStore.getState().items[RESPONSE]?.[0]?.state).toBe('pending'); });
    expect(queueAfterTurn).not.toHaveBeenCalled();
    expect(useChatSteeringStore.getState().own[RESPONSE]?.ids).toEqual(['a']);
    expect(keys).toHaveLength(0);
  });
});

describe('SteeringMessages', () => {
  const ids = (...values: string[]) => new Set(values);

  function Harness(props: { readonly live: boolean; readonly transcript: ReadonlySet<string> }) {
    return (
      <ThemeProvider theme={theme}>
        <SteeringMessages responseMessageId={RESPONSE} transcriptIds={props.transcript} isLive={props.live} />
      </ThemeProvider>
    );
  }

  it('shows pending only while the answer runs, delivered always, and hides a delivered copy that the transcript holds', () => {
    useChatSteeringStore.getState().apply(RESPONSE, [
      { id: 'p', responseMessageId: RESPONSE, authorId: 1, content: 'waiting', state: 'pending', revision: 1, queueOrder: 1, messageId: null },
      { id: 'i', responseMessageId: RESPONSE, authorId: 1, content: 'read', state: 'injected', revision: 1, queueOrder: 2, messageId: 'm-i' },
    ]);
    const { rerender } = render(<Harness live transcript={ids()} />);
    expect(screen.getAllByTestId('chat-steering-status').map((node) => node.textContent)).toEqual(['Waiting for the next step', 'Delivered']);
    rerender(<Harness live={false} transcript={ids()} />);
    expect(screen.getAllByTestId('chat-steering-item')).toHaveLength(1);
    rerender(<Harness live={false} transcript={ids('m-i')} />);
    expect(screen.queryByTestId('chat-steering-messages')).toBeNull();
  });

  it('leaves the reader\'s own pending messages to the composer\'s queue (useOwnPendingSteering)', () => {
    useChatSteeringStore.getState().addOwn(RESPONSE, { projectId: '7', conversationKey: KEY }, EXAMPLE.id);
    act(() => { applySteeringEvent('steer.queued', EXAMPLE); });
    render(<Harness live transcript={ids()} />);
    expect(screen.queryByTestId('chat-steering-messages')).toBeNull();
    const { result, rerender } = renderHook(({ live }) => useOwnPendingSteering(KEY, live), { initialProps: { live: true } });
    expect(result.current.map((item) => item.id)).toEqual([EXAMPLE.id]);
    expect(renderHook(() => useOwnPendingSteering('another', true)).result.current).toEqual([]);
    rerender({ live: false });
    expect(result.current).toEqual([]);
  });

  it('steer.dropped removes the message from the view', () => {
    act(() => { applySteeringEvent('steer.queued', EXAMPLE); });
    act(() => { applySteeringEvent('steer.dropped', { ...EXAMPLE, reason: 'removed' }); });
    expect(useChatSteeringStore.getState().items[RESPONSE]?.[0]?.state).toBe('removed');
    render(<Harness live transcript={ids()} />);
    expect(screen.queryByTestId('chat-steering-messages')).toBeNull();
  });
});

describe('useChatSteering — carry-over to the newer answer', () => {
  // The user sent a newer message before the hand-off: the server moved the leftover message to the newer answer.
  const NEWER = '9e8d7c6b-5a49-4382-9170-6f5e4d3c2b1a';
  const carried = (state: SteeringMessage['state']) => row({ response_message_id: NEWER, revision: EXAMPLE.revision + 1, state });
  const own = () => useChatSteeringStore.getState().own;
  const idsUnder = (answer: string) => useChatSteeringStore.getState().items[answer]?.map((item) => item.id);

  beforeEach(() => {
    useChatSteeringStore.getState().addOwn(RESPONSE, { projectId: '7', conversationKey: KEY }, EXAMPLE.id);
    act(() => { applySteeringEvent('steer.queued', EXAMPLE); });
  });

  it('moves the item and this tab’s ownership (also in sessionStorage) to the newer answer; an old replay does not move it back', () => {
    act(() => { applySteeringEvent('steer.queued', carried('pending')); });
    expect(idsUnder(RESPONSE)).toEqual([]);
    expect(useChatSteeringStore.getState().items[NEWER]).toEqual([expect.objectContaining({ id: EXAMPLE.id, state: 'pending', revision: EXAMPLE.revision + 1 })]);
    expect(own()[RESPONSE]).toBeUndefined();
    expect(own()[NEWER]).toEqual({ projectId: '7', conversationKey: KEY, ids: [EXAMPLE.id] });
    expect(sessionStorage.getItem('el.chat.steeringOwn')).toContain(NEWER);
    act(() => { applySteeringEvent('steer.queued', EXAMPLE); });
    expect(idsUnder(RESPONSE)).toEqual([]);
    expect(own()[NEWER]?.ids).toEqual([EXAMPLE.id]);
  });

  it('keeps the ownership when the settle of the old answer forgot the id before the frame came', () => {
    useChatSteeringStore.getState().forget(RESPONSE, [EXAMPLE.id]);
    useChatSteeringStore.getState().dropOwn(RESPONSE, [EXAMPLE.id]);
    act(() => { applySteeringEvent('steer.queued', carried('pending')); });
    expect(own()[NEWER]?.ids).toEqual([EXAMPLE.id]);
  });

  it('the hand-off of the NEWER answer sends the carried message; the old answer is not handed off', async () => {
    const answers: string[] = [];
    server.use(
      getListChatExecutionSteeringMockHandler(({ params }) => ({ ...getListChatExecutionSteeringResponseMock(), items: params['responseMessageId'] === NEWER ? [carried('turn_ended')] : [] })),
      getHandOffChatExecutionSteeringMockHandler(({ params }) => {
        answers.push(String(params['responseMessageId']));
        return { ...getHandOffChatExecutionSteeringResponseMock(), items: [carried('handed_off')] };
      }),
    );
    const { queueAfterTurn } = setup({ isStreaming: false });
    // The newer turn already ended: the frame comes as turn_ended, and the end-of-turn rule runs again.
    act(() => { applySteeringEvent('steer.queued', carried('turn_ended')); });
    await waitFor(() => { expect(queueAfterTurn).toHaveBeenCalledWith(EXAMPLE.content, expect.any(String)); });
    expect(answers).toEqual([NEWER]);
    expect(queueAfterTurn).toHaveBeenCalledTimes(1);
    await waitFor(() => { expect(own()[NEWER]).toBeUndefined(); });
  });
});
