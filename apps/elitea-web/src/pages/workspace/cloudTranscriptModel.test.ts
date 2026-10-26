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
    expect(view.items[0]).toMatchObject({
      tool: 'open_pull_request',
      remote: true,
      result: { ok: true, summary: 'Pull request #7 opened: https://github.com/o/r/pull/7' },
      link: { href: 'https://github.com/o/r/pull/7', label: 'Open pull request #7' },
    });
    const pushed = JSON.stringify({ status: 'ok', message: 'Pushed 2 commit(s)', branch: 'cloud_v2', url: 'https://gitlab.example.com/g/r/-/tree/cloud_v2' });
    const push = turnViewOf(answer([{ id: 'c2', type: 'tool', name: 'git_push', status: 'complete', toolOutputs: pushed }]));
    expect(push.items[0]).toMatchObject({ result: { summary: 'Pushed 2 commit(s)' }, link: { href: 'https://gitlab.example.com/g/r/-/tree/cloud_v2', label: 'Open branch cloud_v2' } });
  });

  it('keeps any other tool result as it came', () => {
    const view = turnViewOf(answer([{ id: 'c2', type: 'tool', name: 'read_file', status: 'complete', toolOutputs: '{"message":"x"}' }]));
    expect(view.items[0]).toMatchObject({ result: { summary: '{"message":"x"}' } });
  });
});
