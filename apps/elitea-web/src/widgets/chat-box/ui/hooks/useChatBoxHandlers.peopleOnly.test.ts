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

describe('sendQuestion that still has an answerer', () => {
  it('an agent addressed with @Agent keeps its turn and shows the pending answer', async () => {
    const t = setup({ startStreamedExecution: vi.fn(() => Promise.resolve<StreamStartOutcome>({ started: true })) });
    await t.handlers.sendQuestion({ question: '@Aksana @Mira look', isSendingToUser: true, userIds: ['11'], addressedTo: [901] });
    expect(t.setStreamingInfo).toHaveBeenCalledTimes(1);
  });

  it('an agent picked on purpose is an address, so the people it mentions do not turn the message into a people-only one', async () => {
    const t = setup({
      getActiveParticipant: () => ({ id: 901, entity_name: 'application' }),
      startStreamedExecution: vi.fn(() => Promise.resolve<StreamStartOutcome>({ started: true })),
    });
    await t.handlers.sendQuestion({ question: '@Aksana check this', isSendingToUser: true, userIds: ['11'] });
    expect(t.setStreamingInfo).toHaveBeenCalledTimes(1);
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
