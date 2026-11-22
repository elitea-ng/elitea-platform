import { describe, expect, it } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';

import { regeneratingPatch } from './regeneratingPatch';

describe('regeneratingPatch', () => {
  it('clears prior-generation delegationNotices, failureCode, failureCap, and exception while keeping canvas items', () => {
    const prior: ChatMessage = {
      id: 'msg-1',
      role: 'assistant',
      name: 'P0r',
      content: 'old text',
      createdAt: '2026-11-10T00:00:00.000Z',
      exception: 'Execution authorization failed.',
      failureCode: 'AUTHORIZATION_FAILED',
      failureCap: 'max_tool_calls',
      delegationNotices: [
        {
          id: 'pipeline_node:delegate',
          kind: 'refused',
          reason: 'no_grant',
          callerId: 174,
          callerName: 'P0r',
          calleeId: 171,
          calleeName: 'delegate',
          edgeKind: 'pipeline_node',
          missing: [{ kind: 'toolkit', name: 'git' }],
          missingTotal: 1,
          canReview: true,
          editors: [],
        },
      ],
      messageItems: [
        { id: 1, item_type: 'text_message', item_details: { content: 'old text' } },
        { id: 2, item_type: 'canvas_message', item_details: { content: 'canvas' } },
      ] as unknown as ChatMessage['messageItems'],
    };

    const next = regeneratingPatch(prior);
    expect(next.content).toBe('');
    expect(next.exception).toBeUndefined();
    expect(next.failureCode).toBeUndefined();
    expect(next.failureCap).toBeUndefined();
    expect(next.delegationNotices).toBeUndefined();
    expect(next.isStreaming).toBe(true);
    expect(next.isLoading).toBe(true);
    expect(next.messageItems).toHaveLength(1);
    expect((next.messageItems?.[0] as unknown as { item_type?: string })?.item_type).toBe('canvas_message');
  });
});
