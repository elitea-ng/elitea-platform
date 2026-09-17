import { expect, it } from 'vitest';
import { settleInFlight } from './chatStreamSettle';
import { applyChatStreamFrame } from './chatStreamReducer';
import type { ToolAction } from './chatStreamToolAction';
import type { ChatMessage } from './convertMessagesToChatHistory';
import { ROLES } from '@/shared/lib/enums';

it('keeps answer text and independently settles parallel child compaction', () => {
  const original: ChatMessage = { id: 'answer', name: 'Elitea', role: ROLES.Assistant, content: 'Already delivered', createdAt: '2026-09-17', references: ['evidence'] };
  const frame = (call: string, phase: string) => ({
    type: 'agent_context_status', message_id: 'answer', execution_generation: 'generation',
    response_metadata: {
      model_scope: 'agent', context_status: { version: 1, phase },
      parent_agent_path: [{ name: 'Research', call_id: call }],
    },
  });
  let history = applyChatStreamFrame([original], frame('a', 'compacting'));
  history = applyChatStreamFrame(history, frame('b', 'compacting'));
  history = applyChatStreamFrame(history, frame('a', 'compacted'));
  history = applyChatStreamFrame(history, frame('a', 'compacted'));
  const answer = history[0]!;
  expect(answer.content).toBe(original.content);
  expect(answer.references).toEqual(original.references);
  expect(answer.toolActions).toHaveLength(2);
  expect((answer.toolActions as readonly ToolAction[]).map((action) => [action.parent_agent_call_id, action.status])).toEqual([['a', 'complete'], ['b', 'processing']]);
});

it('ordinary measurements do not add transcript actions', () => {
  const history: readonly ChatMessage[] = [];
  expect(applyChatStreamFrame(history, { type: 'agent_context_status', response_metadata: { context_status: { version: 1, phase: 'measured' } } })).toBe(history);
});

it('settles pending notices on both streamed terminal frames and execution failure', () => {
  const original: ChatMessage = { id:'answer', name:'Elitea', role:ROLES.Assistant, content:'Retain this answer', createdAt:'2026-09-17', isStreaming:true };
  const history = applyChatStreamFrame([original], {type:'agent_context_status', message_id:'answer', response_metadata:{context_status:{version:1,phase:'compacting'}}});
  for (const settled of [settleInFlight(history, 'failed'), applyChatStreamFrame(history, {type:'pipeline_finish', message_id:'answer'})]) {
    const action = settled[0]?.toolActions?.[0] as ToolAction;
    expect(action.status).toBe('cancelled');
    expect(action.content).toBe('Compaction stopped before completion.');
  }
});
