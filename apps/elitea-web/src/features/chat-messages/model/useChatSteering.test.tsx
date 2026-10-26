/**
 * Steering during execution (client contract 1.26): where a send goes while a
 * cloud turn runs, the fallbacks of each refusal, and the end-of-turn rule
 * (leftover own messages become ONE next message, then their rows are
 * deleted). The bodies come from the v2.yaml examples through the generated
 * MSW mocks; a refusal is a typed `SteeringError` constant.
 */
import { ThemeProvider } from '@mui/material/styles';
import { act, render, renderHook, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';
import {
  getListChatExecutionSteeringResponseMock,
  getRemoveChatExecutionSteeringMockHandler,
  getSteerChatExecutionMockHandler,
  getSteerChatExecutionResponseMock,
  getUpdateChatExecutionSteeringMockHandler,
  getUpdateChatExecutionSteeringResponseMock,
} from '@/shared/api/generated/chat/chat.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { SteeringError, SteeringMessage } from '@/shared/api/generated/model';
import { server } from '@/test/setup';

import { SteeringMessages } from '../ui/chat-box/SteeringMessages';
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

  it('joins the own turn_ended texts into ONE next message, then deletes their rows', async () => {
    const deleted: string[] = [];
    server.use(getRemoveChatExecutionSteeringMockHandler(({ params }) => { deleted.push(String(params['steeringId'])); }));
    ownAndList(['a', 'b', 'c'], [
      row({ id: 'a', content: 'first', state: 'turn_ended' }),
      row({ id: 'b', content: 'read', state: 'injected', message_id: 'm-b' }),
      row({ id: 'c', content: 'second', state: 'turn_ended' }),
      row({ id: 'x', content: 'not mine', state: 'turn_ended' }),
    ]);
    const { rerender, params, queueAfterTurn } = setup();
    rerender({ ...params, isStreaming: false });
    await waitFor(() => { expect(useChatSteeringStore.getState().own[RESPONSE]).toBeUndefined(); });
    expect(queueAfterTurn).toHaveBeenCalledTimes(1);
    expect(queueAfterTurn).toHaveBeenCalledWith('first\n\nsecond');
    expect(deleted.sort()).toEqual(['a', 'c']);
  });

  it('sends nothing while the server still shows own messages as pending (the turn can still read them)', async () => {
    ownAndList(['a'], [row({ id: 'a', state: 'pending' })]);
    const { rerender, params, queueAfterTurn } = setup();
    rerender({ ...params, isStreaming: false });
    await waitFor(() => { expect(useChatSteeringStore.getState().items[RESPONSE]?.[0]?.state).toBe('pending'); });
    expect(queueAfterTurn).not.toHaveBeenCalled();
    expect(useChatSteeringStore.getState().own[RESPONSE]?.ids).toEqual(['a']);
  });
});

describe('SteeringMessages', () => {
  const ids = (...values: string[]) => new Set(values);

  function Harness(props: { readonly live: boolean; readonly transcript: ReadonlySet<string> }) {
    const model = useChatSteering({ conversationKey: KEY, isStreaming: props.live, streamTarget: () => target, send: vi.fn(), queueAfterTurn: vi.fn() });
    return (
      <ThemeProvider theme={theme}>
        <SteeringMessages responseMessageId={RESPONSE} transcriptIds={props.transcript} isLive={props.live} model={model} />
      </ThemeProvider>
    );
  }

  it('shows pending only while the answer runs, delivered always, and hides a delivered copy that the transcript holds', () => {
    useChatSteeringStore.getState().apply(RESPONSE, [
      { id: 'p', responseMessageId: RESPONSE, authorId: 1, content: 'waiting', state: 'pending', revision: 1, createdAt: '', messageId: null },
      { id: 'i', responseMessageId: RESPONSE, authorId: 1, content: 'read', state: 'injected', revision: 1, createdAt: '', messageId: 'm-i' },
    ]);
    const { rerender } = render(<Harness live transcript={ids()} />);
    expect(screen.getAllByTestId('chat-steering-status').map((node) => node.textContent)).toEqual(['Waiting for the next step', 'Delivered']);
    rerender(<Harness live={false} transcript={ids()} />);
    expect(screen.getAllByTestId('chat-steering-item')).toHaveLength(1);
    rerender(<Harness live={false} transcript={ids('m-i')} />);
    expect(screen.queryByTestId('chat-steering-messages')).toBeNull();
  });

  it('the author edits a pending message in place (PATCH)', async () => {
    const edited = getUpdateChatExecutionSteeringResponseMock();
    let body: unknown;
    server.use(getUpdateChatExecutionSteeringMockHandler(async ({ request }) => { body = await request.json(); return edited; }));
    useChatSteeringStore.getState().addOwn(RESPONSE, { projectId: '7', conversationKey: KEY }, EXAMPLE.id);
    act(() => { applySteeringEvent('steer.queued', EXAMPLE); });
    render(<Harness live transcript={ids()} />);
    await userEvent.click(screen.getByTestId('chat-steering-edit-button'));
    const input = screen.getByRole('textbox', { name: 'Edit the waiting message' });
    await userEvent.clear(input);
    await userEvent.type(input, edited.content);
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => { expect(screen.getByText(edited.content)).toBeTruthy(); });
    expect(body).toEqual({ content: edited.content });
  });

  it('steer.dropped removes the message from the view', () => {
    act(() => { applySteeringEvent('steer.queued', EXAMPLE); });
    act(() => { applySteeringEvent('steer.dropped', { ...EXAMPLE, reason: 'removed' }); });
    expect(useChatSteeringStore.getState().items[RESPONSE]?.[0]?.state).toBe('removed');
    render(<Harness live transcript={ids()} />);
    expect(screen.queryByTestId('chat-steering-messages')).toBeNull();
  });
});
