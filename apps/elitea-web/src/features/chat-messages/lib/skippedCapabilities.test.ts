import { describe, expect, it } from 'vitest';
import { parseSkippedCapabilities, skippedCapabilitiesOf } from './skippedCapabilities';
import { applyChatStreamFrame } from './chatStreamReducer';
import type { ChatStreamFrame } from './chatStreamFrame';
import type { ChatMessage } from './convertMessagesToChatHistory.types';

const toolkit = { kind: 'toolkit', name: 'wiki', type: 'confluence', reason_code: 'unsupported_toolkit_family', reason: 'not available yet' };
const wire = (items: readonly unknown[], more?: number) => ({ items, ...(more ? { more } : {}) });

describe('skipped capabilities', () => {
  it('parses, dedupes and drops malformed entries', () => {
    const parsed = parseSkippedCapabilities(wire([toolkit, toolkit, { kind: 'bogus', name: 'x', reason: 'r' }, { kind: 'toolkit', name: '', reason: 'r' }, null, 'x'], 3));
    expect(parsed).toEqual({ more: 3, items: [{ kind: 'toolkit', name: 'wiki', type: 'confluence', reasonCode: 'unsupported_toolkit_family', reason: 'not available yet' }] });
    expect(parseSkippedCapabilities(undefined).items).toEqual([]);
    expect(parseSkippedCapabilities([toolkit]).items).toEqual([]);
    expect(parseSkippedCapabilities(wire([toolkit], -4)).more).toBe(0);
  });

  // Review round 2: the worker's per-tool entry (`<toolkit>.<tool>`) was dropped.
  it('keeps the worker\'s tool entries', () => {
    const tool = { kind: 'tool', name: 'repo.create_file', type: 'github', reason_code: 'unsupported_tool_selection', reason: 'this tool is not available on this worker yet, so it was left out' };
    expect(parseSkippedCapabilities(wire([tool, toolkit], 69))).toEqual({
      more: 69,
      items: [
        { kind: 'tool', name: 'repo.create_file', type: 'github', reasonCode: 'unsupported_tool_selection', reason: 'this tool is not available on this worker yet, so it was left out' },
        { kind: 'toolkit', name: 'wiki', type: 'confluence', reasonCode: 'unsupported_toolkit_family', reason: 'not available yet' },
      ],
    });
    const many = Array.from({ length: 40 }, (_, index) => ({ ...tool, name: `repo.tool_${index}` }));
    expect(parseSkippedCapabilities(wire(many)).items).toHaveLength(32);
  });

  it('bounds the list', () => {
    const many = Array.from({ length: 80 }, (_, index) => ({ ...toolkit, name: `t${index}` }));
    expect(parseSkippedCapabilities(wire(many)).items).toHaveLength(32);
  });

  it('prefers the live final frame and falls back to the persisted copy', () => {
    expect(skippedCapabilitiesOf({ responseMetadata: { skipped_capabilities: wire([toolkit]) } }).items).toHaveLength(1);
    expect(skippedCapabilitiesOf({ skippedCapabilities: wire([toolkit]) }).items).toHaveLength(1);
    expect(skippedCapabilitiesOf({ responseMetadata: { thread_id: 't' } }).items).toEqual([]);
  });

  it('keeps the list on the message from agent_start and a pause, not only the final frame', () => {
    const base: ChatMessage = { id: 'm1', role: 'assistant', name: 'Agent', content: '', createdAt: '', isStreaming: true };
    const identity = { message_id: 'm1', stream_id: 's', question_id: 'q', sio_event: 'chat_predict', execution_generation: 'g' };
    const start = { ...identity, type: 'agent_start', response_metadata: { skipped_capabilities: wire([toolkit], 2) } } as ChatStreamFrame;
    const history = applyChatStreamFrame([base], start, {});
    expect(skippedCapabilitiesOf(history[0]!).more).toBe(2);
  });
});
