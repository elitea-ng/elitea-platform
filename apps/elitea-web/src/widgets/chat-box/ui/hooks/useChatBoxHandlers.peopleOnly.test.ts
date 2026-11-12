/**
 * `sendQuestion` for a message to people alone: it is delivered (the person's
 * text stays on screen), shows no pending answer, and is never re-sent over
 * the socket as a turn. A message that names an agent, or a message sent with
 * an agent picked, keeps its turn.
 */
import { describe, expect, it, vi } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';

import { useChatBoxHandlers } from './useChatBoxHandlers';

// Plain closures over `deps`, not a React hook (as in `useChatBoxHandlers.test.ts`).
const buildHandlers = useChatBoxHandlers;
import type { ChatBoxHandlerDeps, StreamStartOutcome } from './useChatBoxHandlers.helpers';

const posted: StreamStartOutcome = { started: false, reason: 'posted' };

function setup(overrides: Partial<ChatBoxHandlerDeps> = {}) {
  let history: readonly ChatMessage[] = [];
  const emitSocket = vi.fn(() => true);
  const setStreamingInfo = vi.fn();
  const startStreamedExecution = vi.fn<NonNullable<ChatBoxHandlerDeps['startStreamedExecution']>>(() => Promise.resolve(posted));
  const handlers = buildHandlers({
    emitSocket,
    chatHistory: [],
    setChatHistory: (update) => { history = typeof update === 'function' ? update(history) : update; },
    setStreamingInfo,
    projectId: 1,
    conversationUuid: 'conv-uuid-1',
    startStreamedExecution,
    ...overrides,
  });
  return { handlers, emitSocket, setStreamingInfo, startStreamedExecution, history: () => history };
}

describe('sendQuestion to people alone', () => {
  it('is delivered with no pending answer and no socket turn', async () => {
    const t = setup();
    const result = await t.handlers.sendQuestion({ question: '@Aksana can you see this?', isSendingToUser: true, userIds: ['11'] });

    expect(result.success).toBe(true);
    expect(t.startStreamedExecution).toHaveBeenCalledTimes(1);
    expect(t.setStreamingInfo).not.toHaveBeenCalled();
    expect(t.emitSocket).not.toHaveBeenCalled();
    expect(t.history().filter((message) => message.exception !== undefined)).toHaveLength(0);
    expect(t.history().map((message) => message.content)).toEqual(['@Aksana can you see this?']);
    const payload = t.startStreamedExecution.mock.calls[0]?.[0].payload;
    expect(payload).toMatchObject({ userIds: ['11'] });
    expect(payload).not.toHaveProperty('participant_id');
    expect(payload).not.toHaveProperty('addressedTo');
  });

  it('does the same for @everyone', async () => {
    const t = setup();
    await t.handlers.sendQuestion({ question: '@everyone lunch?', isSendingToUser: true, userIds: ['11', '12'], isMentioningEveryone: true });
    expect(t.setStreamingInfo).not.toHaveBeenCalled();
    expect(t.emitSocket).not.toHaveBeenCalled();
  });
});

describe('a people-only message whose REST start did not deliver it never becomes a turn', () => {
  const outcomes: [string, Partial<ChatBoxHandlerDeps>][] = [
    ['no transport', { startStreamedExecution: vi.fn(() => Promise.resolve<StreamStartOutcome>({ started: false, reason: 'no-transport' })) }],
    ['no start function (the socket-less E2E stack)', { startStreamedExecution: undefined }],
    ['retry-later', { startStreamedExecution: vi.fn(() => Promise.resolve<StreamStartOutcome>({ started: false, reason: 'retry-later' })) }],
    ['a 422 rejection', { startStreamedExecution: vi.fn(() => Promise.resolve<StreamStartOutcome>({ started: false, reason: 'rejected', message: 'refused' })) }],
  ];
  it.each(outcomes)('%s: no chat_predict, a visible failure, the text kept', async (_name, overrides) => {
    const t = setup(overrides);
    const result = await t.handlers.sendQuestion({ question: '@Aksana can you see this?', isSendingToUser: true, userIds: ['11'] });

    expect(result.success).toBe(false);
    expect(t.emitSocket).not.toHaveBeenCalled();
    expect(t.setStreamingInfo).not.toHaveBeenCalled();
    expect(t.history().some((message) => message.exception !== undefined)).toBe(true);
    expect(t.history().some((message) => message.content === '@Aksana can you see this?')).toBe(true);
  });

  it('a message with an answerer still falls back to the socket', async () => {
    const t = setup({ startStreamedExecution: vi.fn(() => Promise.resolve<StreamStartOutcome>({ started: false, reason: 'no-transport' })) });
    await t.handlers.sendQuestion({ question: 'hello' });
    expect(t.emitSocket).toHaveBeenCalledTimes(1);
  });
});

describe('sendQuestion that still has an answerer', () => {
  it('an agent addressed with @Agent keeps its turn and shows the pending answer', async () => {
    const t = setup({ startStreamedExecution: vi.fn(() => Promise.resolve<StreamStartOutcome>({ started: true })) });
    await t.handlers.sendQuestion({ question: '@Aksana @Mira look', isSendingToUser: true, userIds: ['11'], addressedTo: [901] });
    expect(t.setStreamingInfo).toHaveBeenCalledTimes(1);
  });

  it('an agent picked on purpose is an address, so the people it mentions do not turn the message into a people-only one', async () => {
    const start = vi.fn<NonNullable<ChatBoxHandlerDeps['startStreamedExecution']>>(() => Promise.resolve({ started: true }));
    const t = setup({
      getActiveParticipant: () => ({ id: 901, entity_name: 'application' }),
      participants: [{ id: 901, entity_name: 'application' }],
      startStreamedExecution: start,
    });
    await t.handlers.sendQuestion({ question: '@Aksana check this', isSendingToUser: true, userIds: ['11'] });
    expect(t.setStreamingInfo).toHaveBeenCalledTimes(1);
    expect(start.mock.calls[0]?.[0].payload).toMatchObject({ addressedTo: [901] });
  });

  it('a picked agent that is not a participant of this conversation is not an address (Main would 422 the whole message)', async () => {
    const t = setup({
      getActiveParticipant: () => ({ id: 777, entity_name: 'application' }),
      participants: [{ id: 901, entity_name: 'application' }],
    });
    await t.handlers.sendQuestion({ question: '@Aksana check this', isSendingToUser: true, userIds: ['11'] });
    expect(t.setStreamingInfo).not.toHaveBeenCalled();
    expect(t.startStreamedExecution.mock.calls[0]?.[0].payload).not.toHaveProperty('addressedTo');
  });

  it('a picked model is not an address: the message stays people-only', async () => {
    const t = setup({ getActiveParticipant: () => ({ id: 5, entity_name: 'dummy' }) });
    await t.handlers.sendQuestion({ question: '@Aksana check this', isSendingToUser: true, userIds: ['11'] });
    expect(t.setStreamingInfo).not.toHaveBeenCalled();
    expect(t.emitSocket).not.toHaveBeenCalled();
  });

  it('a message with no mention is unchanged', async () => {
    const t = setup({ startStreamedExecution: vi.fn(() => Promise.resolve<StreamStartOutcome>({ started: true })) });
    await t.handlers.sendQuestion({ question: 'hello' });
    expect(t.setStreamingInfo).toHaveBeenCalledTimes(1);
  });
});
