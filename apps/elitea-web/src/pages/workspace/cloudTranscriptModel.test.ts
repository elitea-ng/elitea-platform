import { describe, expect, it } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';

import { turnViewOf } from './cloudTranscriptModel';

function answer(toolActions: unknown[]): ChatMessage {
  return { id: 'm1', role: 'assistant', content: '', toolActions } as unknown as ChatMessage;
}

describe('turnViewOf', () => {
  it("shows an opened pull request as its sentence, with the link, not its JSON", () => {
    const output = JSON.stringify({ status: 'ok', message: 'Pull request #7 opened: https://github.com/o/r/pull/7', pull_request: 7, url: 'https://github.com/o/r/pull/7' });
    const view = turnViewOf(answer([{ id: 'c1', type: 'tool', name: 'open_pull_request', status: 'complete', toolOutputs: output }]));
    expect(view.items[0]).toMatchObject({ tool: 'open_pull_request', remote: true, result: { ok: true, summary: 'Pull request #7 opened: https://github.com/o/r/pull/7' } });
  });

  it('keeps any other tool result as it came', () => {
    const view = turnViewOf(answer([{ id: 'c2', type: 'tool', name: 'read_file', status: 'complete', toolOutputs: '{"message":"x"}' }]));
    expect(view.items[0]).toMatchObject({ result: { summary: '{"message":"x"}' } });
  });
});
