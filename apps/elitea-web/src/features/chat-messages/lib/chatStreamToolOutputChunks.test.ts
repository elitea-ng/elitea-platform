/**
 * Chunked tool output, the LIVE half (#956).
 *
 * The frames below are the shape both workers emit — `agent_tool_output_chunk`
 * events ahead of the completed call, which then carries `tool_output: ""` and
 * `tool_output_chunks {total, tool_output_sha256}`. The claim under test is the
 * one the whole mechanism exists for: what the pin shows is either the WHOLE
 * result or something that says it is partial, and never a prefix presented as
 * the result.
 */
import { describe, expect, it } from 'vitest';

import { sha256Hex } from '@/shared/lib/hash/sha256';

import { applyChatStreamFrame, type ChatStreamContext, type ToolAction } from './chatStreamReducer';
import { SocketMessageType } from './chatStreamFrame';
import type { ChatMessage } from './convertMessagesToChatHistory';

const MESSAGE_ID = '63c6d989-2860-5d68-9e3e-3587c63350d3';
const QUESTION_ID = '11111111-2222-3333-4444-555555555555';
const RUN_ID = 'call-1';
const CONTEXT: ChatStreamContext = { name: 'Agent', now: () => '2026-09-20T00:00:00.000Z' };

/** ~80k characters: the artifact read #956 was filed for. */
const OUTPUT = 'AUTOTESTMED the quick brown fox jumps over the lazy dog 0123456789\n'.repeat(1_213);
const DIGEST = sha256Hex(OUTPUT);
const CHUNK_SIZE = 30 * 1024;

function slices(text: string, size: number): readonly string[] {
  const parts: string[] = [];
  for (let offset = 0; offset < text.length; offset += size) parts.push(text.slice(offset, offset + size));
  return parts;
}

function pendingAssistant(): ChatMessage {
  return {
    id: MESSAGE_ID,
    role: 'assistant',
    name: 'Agent',
    content: '',
    createdAt: '2026-09-20T00:00:00.000Z',
    questionId: QUESTION_ID,
    isStreaming: true,
    isLoading: true,
  };
}

function frame(type: string, extra: Record<string, unknown> = {}) {
  return { type, message_id: MESSAGE_ID, question_id: QUESTION_ID, stream_id: 's-1', ...extra };
}

function toolStart() {
  return frame(SocketMessageType.AgentToolStart, {
    response_metadata: { tool_run_id: RUN_ID, tool_name: 'read_file', tool_inputs: { filename: 'big.txt' } },
  });
}

function chunkFrame(text: string, index: number, total: number, digest = DIGEST) {
  return frame(SocketMessageType.AgentToolOutputChunk, {
    content: text,
    response_metadata: {
      tool_output_chunk: { tool_call_id: RUN_ID, index, total, tool_output_sha256: digest },
    },
  });
}

function completion(total: number, digest = DIGEST) {
  return frame(SocketMessageType.AgentToolEnd, {
    response_metadata: {
      tool_run_id: RUN_ID,
      tool_name: 'read_file',
      tool_output: '',
      tool_output_chunks: { total, tool_output_sha256: digest },
      timestamp_finish: '2026-09-20T00:00:03.000Z',
    },
  });
}

function reduce(frames: readonly Record<string, unknown>[]): readonly ChatMessage[] {
  return frames.reduce(
    (history, next) => applyChatStreamFrame(history, next, CONTEXT),
    [pendingAssistant()] as readonly ChatMessage[],
  );
}

function pin(history: readonly ChatMessage[]): ToolAction | undefined {
  return ((history[0]?.toolActions ?? []) as readonly ToolAction[]).find((action) => action.id === RUN_ID);
}

describe('chunked tool output in the live stream', () => {
  it('assembles an 80k result the pin can show, in order', () => {
    const parts = slices(OUTPUT, CHUNK_SIZE);
    expect(parts.length).toBeGreaterThan(2);
    const history = reduce([
      toolStart(),
      ...parts.map((text, index) => chunkFrame(text, index, parts.length)),
      completion(parts.length),
    ]);

    const action = pin(history);
    expect(action?.toolOutputs).toBe(OUTPUT);
    expect(action?.['toolOutputPartial']).toBe(false);
    expect(action?.status).toBe('complete');
  });

  it('shows the assembled prefix while the rest is still arriving', () => {
    const parts = slices(OUTPUT, CHUNK_SIZE);
    const history = reduce([toolStart(), chunkFrame(parts[0] ?? '', 0, parts.length)]);
    // Not the whole result and not nothing: the part that is contiguous from
    // the start, so a large read is visibly streaming rather than blank.
    expect(pin(history)?.toolOutputs).toBe(parts[0]);
  });

  it('assembles by INDEX, so an out-of-order delivery is not spliced together wrongly', () => {
    const parts = slices(OUTPUT, CHUNK_SIZE);
    const shuffled = [...parts.keys()].reverse();
    const history = reduce([
      toolStart(),
      ...shuffled.map((index) => chunkFrame(parts[index] ?? '', index, parts.length)),
      completion(parts.length),
    ]);

    const action = pin(history);
    expect(action?.toolOutputs).toBe(OUTPUT);
    expect(action?.['toolOutputPartial']).toBe(false);
  });

  it('never shows a hole as a result: a prefix is shown only up to the gap', () => {
    const parts = slices(OUTPUT, CHUNK_SIZE);
    // Chunk 1 is missing; 0 and 2 arrive. Joining what arrived would splice
    // the third slice onto the first and read like a real document.
    const history = reduce([
      toolStart(),
      chunkFrame(parts[0] ?? '', 0, parts.length),
      chunkFrame(parts[2] ?? '', 2, parts.length),
    ]);
    expect(pin(history)?.toolOutputs).toBe(parts[0]);
  });

  it('marks a LOST chunk partial rather than presenting a short result', () => {
    const parts = slices(OUTPUT, CHUNK_SIZE);
    const history = reduce([
      toolStart(),
      ...parts.slice(0, -1).map((text, index) => chunkFrame(text, index, parts.length)),
      completion(parts.length),
    ]);

    const action = pin(history);
    expect(action?.['toolOutputPartial']).toBe(true);
    const shown = action?.toolOutputs;
    expect(typeof shown).toBe('string');
    expect(String(shown).length).toBeLessThan(OUTPUT.length);
    expect(shown).not.toBe(OUTPUT);
  });

  it('marks a TAMPERED assembly partial: the digest is checked, not assumed', () => {
    const parts = slices(OUTPUT, CHUNK_SIZE);
    const corrupted = [...parts];
    corrupted[1] = 'x'.repeat((parts[1] ?? '').length);
    const history = reduce([
      toolStart(),
      ...corrupted.map((text, index) => chunkFrame(text, index, parts.length)),
      completion(parts.length),
    ]);

    const action = pin(history);
    expect(action?.['toolOutputPartial']).toBe(true);
  });

  it('ignores a chunk for a tool call this message never started', () => {
    const before = [pendingAssistant()] as readonly ChatMessage[];
    const after = applyChatStreamFrame(before, chunkFrame('orphan', 0, 1, sha256Hex('orphan')), CONTEXT);
    // Same reference: an unattachable chunk must be inert, not a partial write.
    expect(after).toBe(before);
  });

  it('leaves an ordinary inline tool result exactly as it was', () => {
    const history = reduce([
      toolStart(),
      frame(SocketMessageType.AgentToolEnd, {
        response_metadata: { tool_run_id: RUN_ID, tool_name: 'read_file', tool_output: 'a short result' },
      }),
    ]);
    const action = pin(history);
    expect(action?.toolOutputs).toBe('a short result');
    expect(action?.['toolOutputPartial']).toBeUndefined();
    expect(action?.['toolOutputChunks']).toBeUndefined();
  });

  it('is idempotent on a repeated chunk, which a reconnect replays', () => {
    const parts = slices(OUTPUT, CHUNK_SIZE);
    const history = reduce([
      toolStart(),
      ...parts.map((text, index) => chunkFrame(text, index, parts.length)),
      chunkFrame(parts[0] ?? '', 0, parts.length),
      completion(parts.length),
    ]);
    expect(pin(history)?.toolOutputs).toBe(OUTPUT);
    expect(pin(history)?.['toolOutputPartial']).toBe(false);
  });
});
