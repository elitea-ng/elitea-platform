/**
 * FU-335: the delegation-refusal notice, from the live frame and from a
 * reload, with its author-written toolkit name bounded and inert.
 */
import { describe, expect, it } from 'vitest';

import type { MessageGroupWire } from '@/entities/message/lib/wire';
import { ROLES } from '@/shared/lib/enums';

import { applyChatStreamFrame } from './chatStreamReducer';
import { recordStreamFailure } from './chatStreamSettle';
import type { ChatStreamFrame } from './chatStreamFrame';
import { convertMessagesToChatHistory } from './convertMessagesToChatHistory';
import type { ChatMessage } from './convertMessagesToChatHistory.types';
import { MAX_DELEGATION_TOOLKIT_NAME_CHARS, parseDelegationRefusal } from './delegationRefusal';

const SENTENCE = 'This run did not start because a saved agent or pipeline it runs needs a toolkit, tool or other grant that its caller does not hold; attach it to the caller or remove it from that agent or pipeline.';
const CONTEXT = { name: 'Agent', now: () => '2026-11-13T00:00:00.000Z' };
const question: ChatMessage = { id: 'q-1', role: ROLES.User, name: 'Alice', content: 'go', createdAt: '2026-11-13T00:00:00.000Z' };

function noticeFrame(block: unknown): ChatStreamFrame {
  return {
    type: 'agent_delegation_refused',
    message_id: 'answer-1',
    content: null,
    response_metadata: { delegation_refused: block },
  } as unknown as ChatStreamFrame;
}

describe('delegation refusal notice', () => {
  it('parses the node kind and the toolkit, and drops malformed blocks', () => {
    expect(parseDelegationRefusal({ code: 'DELEGATION_GRANT_MISSING', node: 'agent', toolkit: 'batch' })).toEqual({ node: 'agent', toolkit: 'batch' });
    expect(parseDelegationRefusal({ node: 'pipeline', toolkit: null })).toEqual({ node: 'pipeline' });
    expect(parseDelegationRefusal({ node: 'tool', toolkit: 'batch' })).toBeUndefined();
    expect(parseDelegationRefusal(['agent'])).toBeUndefined();
    expect(parseDelegationRefusal(undefined)).toBeUndefined();
  });

  it(`bounds the toolkit name at ${MAX_DELEGATION_TOOLKIT_NAME_CHARS} characters`, () => {
    const atLimit = 'x'.repeat(MAX_DELEGATION_TOOLKIT_NAME_CHARS);
    expect(parseDelegationRefusal({ node: 'agent', toolkit: atLimit })?.toolkit).toBe(atLimit);
    const overLimit = 'y'.repeat(MAX_DELEGATION_TOOLKIT_NAME_CHARS + 1);
    expect(parseDelegationRefusal({ node: 'agent', toolkit: overLimit })?.toolkit).toBe('y'.repeat(MAX_DELEGATION_TOOLKIT_NAME_CHARS));
  });

  it('keeps the name exactly as written, markup included, as text', () => {
    const shown = (toolkit: string) => parseDelegationRefusal({ node: 'agent', toolkit })?.toolkit;
    // Review 1 item 4: a tag-like part of a legitimate name is kept.
    expect(shown('Jira <prod>')).toBe('Jira <prod>');
    expect(shown('<script>alert(1)</script>Jira')).toBe('<script>alert(1)</script>Jira');
    expect(shown('[Jira](javascript:alert(1))')).toBe('[Jira](javascript:alert(1))');
    expect(shown('Team & Co')).toBe('Team & Co');
    expect(shown('  Team\n\tGitHub  ')).toBe('Team GitHub');
    expect(shown('   ')).toBeUndefined();
  });

  it('keeps the live notice on the answer the failure then settles', () => {
    const live = applyChatStreamFrame([question], noticeFrame({ node: 'agent', toolkit: 'batch' }), CONTEXT);
    expect(live).toHaveLength(2);
    expect(live[1]?.delegationRefusal).toEqual({ node: 'agent', toolkit: 'batch' });
    const failed = recordStreamFailure(live, SENTENCE, CONTEXT, 'q-1', 'answer-1', 'AUTHORIZATION_FAILED');
    expect(failed).toHaveLength(2);
    expect(failed[1]).toMatchObject({ exception: SENTENCE, failureCode: 'AUTHORIZATION_FAILED', isStreaming: false });
    expect(parseDelegationRefusal(failed[1]?.delegationRefusal)).toEqual({ node: 'agent', toolkit: 'batch' });
  });

  it('ignores a malformed notice frame', () => {
    const history = [question];
    expect(applyChatStreamFrame(history, noticeFrame({ node: 'other' }), CONTEXT)).toBe(history);
  });

  it('restores the persisted notice on reload', () => {
    const [answer] = convertMessagesToChatHistory([{
      id: 1, uuid: 'answer-1', role: 'assistant', content: '', created_at: '2026-11-13T00:00:00Z',
      meta: { is_error: true, error: SENTENCE, error_code: 'AUTHORIZATION_FAILED', delegation_refused: { node: 'agent', toolkit: 'batch' } },
    }] as unknown as readonly MessageGroupWire[], []);
    expect(answer?.exception).toBe(SENTENCE);
    expect(parseDelegationRefusal(answer?.delegationRefusal)).toEqual({ node: 'agent', toolkit: 'batch' });
  });
});
