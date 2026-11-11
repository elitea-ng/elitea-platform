/**
 * Client contract 1.48 (ADR-0050 decision 4): a cloud answer's temporary
 * sub-agents, live (`agent_subagent` frames) and after a reload
 * (`meta.subagents_v1`), fold into the same `subAgents`.
 */
import { describe, expect, it } from 'vitest';

import type { MessageGroupWire } from '@/entities/message/lib/wire';
import { socketMessageSchema } from '@/shared/api/socket/messages';

import { applyChatStreamFrame, type ChatStreamContext } from './chatStreamReducer';
import { resetTurnForReplay } from './chatStreamReattach';
import { settleInFlight } from './chatStreamSettle';
import { HANDLED_STREAM_TYPES } from './chatStreamFrame';
import { convertMessagesToChatHistory, type ChatMessage } from './convertMessagesToChatHistory';

const MESSAGE_ID = '63c6d989-2860-5d68-9e3e-3587c63350d3';
const CONTEXT: ChatStreamContext = { name: 'Agent', now: () => '2026-11-10T00:00:00.000Z' };
const pending: ChatMessage = { id: MESSAGE_ID, role: 'assistant', name: 'Agent', content: '', createdAt: '2026-11-10T00:00:00.000Z', isStreaming: true, isLoading: true };

const frame = (entry: Record<string, unknown>) => ({ type: 'agent_subagent', message_id: MESSAGE_ID, response_metadata: { subagent_v1: entry } }) as const;

const started = { kind: 'started', agent_id: 'a_1', parent_call_id: 'call-spawn', ordinal: 1, label: 'callers', display_name: 'Owl', avatar: 'default:owl' };

describe('agent_subagent on the live stream', () => {
  it('is a handled, schema-valid frame', () => {
    expect(HANDLED_STREAM_TYPES.has('agent_subagent')).toBe(true);
    expect(socketMessageSchema.safeParse(frame(started)).success).toBe(true);
  });

  it('folds a child into the answer lane by lane, never into its text or tool actions', () => {
    let history: readonly ChatMessage[] = [pending];
    for (const entry of [
      started,
      { kind: 'tool_start', agent_id: 'a_1', call_id: 'k1', tool: 'grep', args_summary: 'run_agent' },
      { kind: 'tool_end', agent_id: 'a_1', call_id: 'k1', ok: true, summary: '2 matches' },
      { kind: 'finished', agent_id: 'a_1', status: 'completed', steps: 3, input_tokens: 1000, output_tokens: 200, result: 'Two callers.' },
    ]) {
      history = applyChatStreamFrame(history, frame(entry), CONTEXT);
    }
    const answer = history[0];
    expect(answer?.content).toBe('');
    expect(answer?.toolActions).toBeUndefined();
    expect(answer?.subAgents).toEqual([
      expect.objectContaining({
        agentId: 'a_1',
        displayName: 'Owl',
        avatar: 'default:owl',
        status: 'completed',
        result: 'Two callers.',
        items: [{ type: 'tool', callId: 'k1', tool: 'grep', argsSummary: 'run_agent', ok: true, summary: '2 matches' }],
      }),
    ]);
  });

  it('keeps the same history for an entry that does not apply', () => {
    const history = [pending];
    expect(applyChatStreamFrame(history, frame({ kind: 'text', agent_id: 'unknown', text: 'x' }), CONTEXT)).toBe(history);
  });

  it('a turn that ends with a child still running stops it', () => {
    const live = applyChatStreamFrame([pending], frame(started), CONTEXT);
    const ended = applyChatStreamFrame(live, { type: 'agent_response', message_id: MESSAGE_ID, content: 'done', response_metadata: { finish_reason: 'stop' } }, CONTEXT);
    expect(ended[0]?.subAgents?.[0]?.status).toBe('cancelled');
  });

  it('a stream that fails or stops without a terminal frame stops a running child too', () => {
    const live = applyChatStreamFrame([pending], frame(started), CONTEXT);
    expect(settleInFlight(live, 'cut')[0]?.subAgents?.[0]?.status).toBe('cancelled');
  });

  it('a replay from cursor 0 rebuilds the seeded lanes once, never doubling their text', () => {
    const text = frame({ kind: 'text', agent_id: 'a_1', text: 'Looking.' });
    const seeded = applyChatStreamFrame(applyChatStreamFrame([pending], frame(started), CONTEXT), text, CONTEXT);
    let replayed = resetTurnForReplay(seeded, MESSAGE_ID);
    expect(replayed[0]?.subAgents).toBeUndefined();
    replayed = applyChatStreamFrame(applyChatStreamFrame(replayed, frame(started), CONTEXT), text, CONTEXT);
    expect(replayed[0]?.subAgents?.[0]?.items).toEqual(seeded[0]?.subAgents?.[0]?.items);
  });
});

describe('meta.subagents_v1 after a reload', () => {
  const lanes = [
    { agent_id: 'a_1', parent_call_id: 'call-spawn', ordinal: 1, display_name: 'Owl', status: 'completed', items: [{ type: 'text', text: 'Found it.' }], result: 'Found it.' },
    { agent_id: 'a_2', parent_call_id: 'call-spawn', ordinal: 2, status: 'running', items: [] },
  ];
  const row = (streaming: boolean): MessageGroupWire =>
    ({ id: '4', uuid: MESSAGE_ID, role: 'assistant', content: 'ok', created_at: '2026-11-10 10:00:05', is_streaming: streaming, meta: { subagents_v1: lanes } }) as unknown as MessageGroupWire;

  it('reads the stored lanes, a settled answer stopping a child left running', () => {
    const [answer] = convertMessagesToChatHistory([row(false)], []);
    expect(answer?.subAgents?.map((lane) => [lane.agentId, lane.displayName, lane.status])).toEqual([
      ['a_1', 'Owl', 'completed'],
      ['a_2', undefined, 'cancelled'],
    ]);
  });

  it('a streaming answer keeps a running child running', () => {
    const [answer] = convertMessagesToChatHistory([row(true)], []);
    expect(answer?.subAgents?.[1]?.status).toBe('running');
  });

  it('an answer without lanes has none', () => {
    const [answer] = convertMessagesToChatHistory([{ ...row(false), meta: {} } as MessageGroupWire], []);
    expect(answer?.subAgents).toBeUndefined();
  });
});
