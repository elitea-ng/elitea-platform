/**
 * Client contract 1.38 on the live stream: the run's `participant_id` names
 * the answer's avatar; `agent_state` never touches the transcript.
 */
import { describe, expect, it } from 'vitest';

import { applyChatStreamFrame, type ChatStreamContext } from './chatStreamReducer';
import type { ChatMessage } from './convertMessagesToChatHistory';

const MESSAGE_ID = '63c6d989-2860-5d68-9e3e-3587c63350d3';
const CONTEXT: ChatStreamContext = { name: 'Agent', now: () => '2026-08-13T00:00:00.000Z' };
const pending: ChatMessage = { id: MESSAGE_ID, role: 'assistant', name: 'Agent', content: '', createdAt: '2026-08-13T00:00:00.000Z', isStreaming: true, isLoading: true };

describe('the answer participant', () => {
  it('stamps the participant of the run on its answer', () => {
    const next = applyChatStreamFrame([pending], { type: 'agent_llm_chunk', message_id: MESSAGE_ID, content: 'Hi', participant_id: 4, addressed_to: [] }, CONTEXT);
    expect(next[0]?.authorParticipantId).toBe(4);
    expect(next[0]?.content).toBe('Hi');
  });

  it('keeps the same array when the participant is already stamped or absent', () => {
    const stamped = [{ ...pending, authorParticipantId: 4 }];
    const frame = { type: 'agent_tool_start', message_id: MESSAGE_ID, participant_id: 4, response_metadata: { tool_run_id: 'r1', tool_name: 'x' } } as const;
    expect(applyChatStreamFrame(stamped, frame, CONTEXT)[0]?.authorParticipantId).toBe(4);
    const unnamed = applyChatStreamFrame([pending], { type: 'agent_llm_chunk', message_id: MESSAGE_ID, content: 'Hi' }, CONTEXT);
    expect(unnamed[0]?.authorParticipantId).toBeUndefined();
  });

  it('agent_state only names the answering participant', () => {
    const history = [pending];
    const next = applyChatStreamFrame(history, { type: 'agent_state', message_id: MESSAGE_ID, participant_id: 4, state: 'thinking' }, CONTEXT);
    expect(next[0]).toEqual({ ...pending, authorParticipantId: 4 });
    expect(applyChatStreamFrame(next, { type: 'agent_state', message_id: MESSAGE_ID, participant_id: 4, state: 'speaking' }, CONTEXT)).toBe(next);
  });
});
