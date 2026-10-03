import { describe, expect, it } from 'vitest';

import type { ChatMessage } from './convertMessagesToChatHistory';
import { executionEventsPath, findReattachableTurn, resetTurnForReplay } from './chatStreamReattach';

const AT = '2026-10-03T00:00:00.000Z';
const question: ChatMessage = { id: 'q1', role: 'user', name: 'Alice', content: 'hi', createdAt: AT };
const inFlight: ChatMessage = {
  id: 'a1', role: 'assistant', name: 'Agent', content: '...', createdAt: AT,
  isStreaming: true, isLoading: true, taskId: 'exec-1', questionId: 'q1',
  toolActions: [{ id: 'tool-1' } as never],
};

describe('findReattachableTurn (#6654)', () => {
  it('finds a last assistant turn that is in flight with an execution', () => {
    expect(findReattachableTurn([question, inFlight])).toStrictEqual({ messageId: 'a1', executionId: 'exec-1', questionId: 'q1' });
  });

  it('ignores a settled turn, a turn with no execution, and an older in-flight row', () => {
    expect(findReattachableTurn([question, { ...inFlight, isStreaming: false }])).toBeUndefined();
    expect(findReattachableTurn([question, { ...inFlight, taskId: undefined }])).toBeUndefined();
    expect(findReattachableTurn([inFlight, question])).toBeUndefined();
    expect(findReattachableTurn([])).toBeUndefined();
  });
});

describe('resetTurnForReplay', () => {
  it('clears what the seed drew, so the replay builds the turn once', () => {
    const next = resetTurnForReplay([question, inFlight], 'a1');
    expect(next[1]).toMatchObject({ content: '', toolActions: [], isStreaming: true });
    expect(next[0]).toBe(question);
  });

  it('returns the same history when the turn is not there', () => {
    const history = [question];
    expect(resetTurnForReplay(history, 'a1')).toBe(history);
  });
});

describe('executionEventsPath', () => {
  it('builds the stream path every start route answers with', () => {
    expect(executionEventsPath(7, 'exec-1')).toBe('/api/v2/executions/7/exec-1/events');
  });
});
