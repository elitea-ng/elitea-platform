import { describe, expect, it } from 'vitest';

import { applyChatStreamFrame, type ToolAction } from './chatStreamReducer';
import { SocketMessageType, type ChatStreamFrame } from './chatStreamFrame';
import type { ChatMessage } from './convertMessagesToChatHistory';

const parent: ChatMessage = {
  id: 'parent-answer',
  role: 'assistant',
  name: 'Full Name Resolver',
  content: 'I will check both names.',
  createdAt: '2026-09-06T00:00:00Z',
  isStreaming: true,
  isLoading: false,
};
const owner = {
  parent_agent_name: 'Name Resolver',
  parent_agent_call_id: 'name-call',
  parent_agent_path: [{ name: 'Name Resolver', call_id: 'name-call' }],
};

describe('child model output ownership', () => {
  it.each([owner, { metadata: owner }, { tool_meta: { metadata: owner } }])(
    'does not append a child answer or finish its parent: %j',
    (metadata) => {
      const frames: ChatStreamFrame[] = [
        { type: SocketMessageType.AgentLlmStart },
        { type: SocketMessageType.AgentLlmChunk, content: 'The name Ada means...' },
        { type: SocketMessageType.AgentLlmEnd },
        {
          type: SocketMessageType.AgentResponse,
          content: 'The name Ada means...',
          response_metadata: { finish_reason: 'stop' },
        },
        { type: SocketMessageType.PipelineFinish },
      ];
      const result = frames.reduce(
        (history, frame) => applyChatStreamFrame(history, {
          ...frame,
          message_id: parent.id,
          response_metadata: { ...frame.response_metadata, ...metadata },
        }),
        [parent] as readonly ChatMessage[],
      );
      expect(result[0]?.content).toBe(parent.content);
      expect(result[0]?.isStreaming).toBe(true);
      expect(result[0]?.isLoading).toBe(false);
      const completed = applyChatStreamFrame(result, {
        type: SocketMessageType.AgentResponse,
        message_id: parent.id,
        content: 'Both names are resolved.',
        response_metadata: { finish_reason: 'stop' },
      });
      expect(completed[0]?.content).toContain('Both names are resolved.');
      expect(completed[0]?.isStreaming).toBe(false);
    },
  );

  it('retains completed child steps without settling the parent reasoning or loading state', () => {
    const childStep: ToolAction = { id: 'child-step', name: 'Thinking step', type: 'tool', status: 'processing', ...owner };
    const message: ChatMessage = {
      ...parent,
      isLoading: true,
      toolActions: [childStep],
    };
    const result = applyChatStreamFrame([message], {
      type: SocketMessageType.AgentLlmEnd,
      message_id: parent.id,
      response_metadata: {
        ...owner,
        tool_run_id: 'child-step',
        thinking_steps: [{ tool_run_id: 'child-step', text: 'Completed child result', ...owner }],
      },
    });
    expect(result[0]?.content).toBe(parent.content);
    expect(result[0]?.isLoading).toBe(true);
    expect(result[0]?.isStreaming).toBe(true);
    expect(result[0]?.toolActions?.[0]).toMatchObject({ content: 'Completed child result', status: 'complete' });
  });
});
