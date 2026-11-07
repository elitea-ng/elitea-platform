import { describe, expect, it } from 'vitest';

import type { ChatMessage } from './convertMessagesToChatHistory';
import { executionEventsPath, executionIdOfEvents, findReattachableTurn, resetTurnForReplay, stampExecution } from './chatStreamReattach';

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

  it.each(['..', '.', 'a/b', '%2e%2e', '../events', 'exec 1', 'exec?x=1', 'exec#1', '-exec', 'x'.repeat(257)])(
    'refuses a task_id that is not an execution id: %s',
    (taskId) => {
      expect(findReattachableTurn([question, { ...inFlight, taskId }])).toBeUndefined();
    },
  );

  it('accepts the hex execution id Main mints', () => {
    const taskId = '0123456789abcdef0123456789abcdef';
    expect(findReattachableTurn([question, { ...inFlight, taskId }])?.executionId).toBe(taskId);
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

  it.each(['..', '.', 'a/b', '%2e%2e', ''])('builds no path for %s', (executionId) => {
    expect(executionEventsPath(7, executionId)).toBeUndefined();
  });

  it('keeps every built path under the project executions prefix', () => {
    for (const executionId of ['exec-1', 'A_b-9']) {
      const path = executionEventsPath('7', executionId);
      expect(path).toBeDefined();
      expect(new URL(path ?? '', 'https://elitea.test').pathname).toBe(`/api/v2/executions/7/${executionId}/events`);
    }
  });
});

describe('the live answer names its run (#1598)', () => {
  it('reads the execution id out of an events path, refusing an odd one', () => {
    expect(executionIdOfEvents('/api/v2/executions/7/exec-1/events')).toBe('exec-1');
    expect(executionIdOfEvents('/api/v2/executions/7/..%2Fx/events')).toBeUndefined();
    expect(executionIdOfEvents('/api/v2/executions/7/%E0%A4%A/events')).toBeUndefined();
    expect(executionIdOfEvents(undefined)).toBeUndefined();
  });

  it('stamps executionId on the response row only, never taskId, and keeps the array when nothing changes', () => {
    const history = [question, { ...inFlight, taskId: undefined }];
    const stamped = stampExecution(history, 'a1', 'exec-9');
    expect(stamped[1]?.executionId).toBe('exec-9');
    // Chain follow reads taskId differing from a read row's as a resumed answer.
    expect(stamped[1]?.taskId).toBeUndefined();
    expect(stamped[0]).toBe(question);
    expect(stampExecution(stamped, 'a1', 'exec-9')).toBe(stamped);
    expect(stampExecution(history, 'missing', 'exec-9')).toBe(history);
    expect(stampExecution(history, 'a1', undefined)).toBe(history);
  });
});
