/**
 * Steering (client contract 1.27): a turn that the user STOPPED, or that
 * FAILED. The server never continues such a turn, and the client does not
 * send its `turn_ended` messages by itself: it offers them. "Send" hands them
 * off with a question id and sends them with that same id; "Dismiss" leaves
 * them on the server. The bodies come from the v2.yaml examples through the
 * generated MSW mocks.
 */
import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  getHandOffChatExecutionSteeringMockHandler,
  getHandOffChatExecutionSteeringResponseMock,
  getListChatExecutionSteeringMockHandler,
  getListChatExecutionSteeringResponseMock,
  getRemoveChatExecutionSteeringMockHandler,
  getSteerChatExecutionResponseMock,
} from '@/shared/api/generated/chat/chat.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { SteeringMessage } from '@/shared/api/generated/model';
import { server } from '@/test/setup';

import { useChatSteeringStore } from './chatSteering.store';
import { useChatSteering, type UseChatSteeringParams } from './useChatSteering';

const BASE = '/api/v2';
const EXAMPLE = getSteerChatExecutionResponseMock();
const RESPONSE = EXAMPLE.response_message_id;
const KEY = 'conv-offer';
const target = { projectId: 7, messageGroupUuid: RESPONSE };
const row = (patch: Partial<SteeringMessage>): SteeringMessage => ({ ...EXAMPLE, ...patch });

/** Two own messages that the turn did not read, and the hand-off calls (with their `question_id`). */
function leftovers() {
  for (const id of ['a', 'b']) useChatSteeringStore.getState().addOwn(RESPONSE, { projectId: '7', conversationKey: KEY }, id);
  const items = [row({ id: 'a', content: 'first', state: 'turn_ended', queue_order: 1 }), row({ id: 'b', content: 'second', state: 'turn_ended', queue_order: 2 })];
  const questions: string[] = [];
  server.use(
    getListChatExecutionSteeringMockHandler(() => ({ ...getListChatExecutionSteeringResponseMock(), items })),
    getHandOffChatExecutionSteeringMockHandler(async ({ request }) => {
      questions.push(((await request.json()) as { question_id: string }).question_id);
      return { ...getHandOffChatExecutionSteeringResponseMock(), items: items.map((item) => ({ ...item, state: 'handed_off' })) };
    }),
  );
  return questions;
}

function setup(overrides: Partial<UseChatSteeringParams> = {}) {
  const queueAfterTurn = vi.fn();
  const params: UseChatSteeringParams = { conversationKey: KEY, isStreaming: true, streamTarget: () => target, send: vi.fn(), queueAfterTurn, ...overrides };
  const hook = renderHook((props: UseChatSteeringParams) => useChatSteering(props), { initialProps: params });
  return { ...hook, queueAfterTurn, params };
}

beforeEach(() => {
  sessionStorage.clear();
  useChatSteeringStore.resetForTests();
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('useChatSteering — a stopped turn', () => {
  it('offers the leftover texts and does not hand them off; Send hands off with a question id and sends with it', async () => {
    const questions = leftovers();
    const { result, rerender, params, queueAfterTurn } = setup();
    act(() => { result.current.onStop(); });
    expect(useChatSteeringStore.getState().own[RESPONSE]?.held).toBe(true);
    rerender({ ...params, isStreaming: false });
    await waitFor(() => { expect(result.current.offer).toEqual({ responseMessageId: RESPONSE, text: 'first\n\nsecond' }); });
    expect(questions).toEqual([]);
    expect(queueAfterTurn).not.toHaveBeenCalled();
    act(() => { result.current.acceptOffer(); });
    await waitFor(() => { expect(queueAfterTurn).toHaveBeenCalledTimes(1); });
    expect(questions).toHaveLength(1);
    expect(queueAfterTurn).toHaveBeenCalledWith('first\n\nsecond', questions[0]);
    expect(result.current.offer).toBeNull();
    await waitFor(() => { expect(useChatSteeringStore.getState().own[RESPONSE]).toBeUndefined(); });
  });

  it('offers again after a reload (the record keeps `held`)', async () => {
    leftovers();
    const first = setup();
    act(() => { first.result.current.onStop(); });
    first.unmount();
    useChatSteeringStore.resetForTests();
    const { result } = setup({ isStreaming: false });
    await waitFor(() => { expect(result.current.offer?.text).toBe('first\n\nsecond'); });
  });
});

describe('useChatSteering — a failed turn', () => {
  it('offers the texts; Dismiss leaves them on the server (no hand-off, no DELETE) and forgets them here', async () => {
    const deleted = vi.fn();
    server.use(getRemoveChatExecutionSteeringMockHandler(() => { deleted(); }));
    const questions = leftovers();
    const { result, queueAfterTurn } = setup({ isStreaming: false, answerFailed: (id) => id === RESPONSE });
    await waitFor(() => { expect(result.current.offer?.text).toBe('first\n\nsecond'); });
    act(() => { result.current.dismissOffer(); });
    expect(result.current.offer).toBeNull();
    expect(useChatSteeringStore.getState().own[RESPONSE]).toBeUndefined();
    expect(questions).toEqual([]);
    expect(deleted).not.toHaveBeenCalled();
    expect(queueAfterTurn).not.toHaveBeenCalled();
  });

  it('a successful turn still hands off by itself', async () => {
    const questions = leftovers();
    const { result, queueAfterTurn } = setup({ isStreaming: false, answerFailed: () => false });
    await waitFor(() => { expect(queueAfterTurn).toHaveBeenCalledWith('first\n\nsecond', questions[0]); });
    expect(result.current.offer).toBeNull();
  });
});
