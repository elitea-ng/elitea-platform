/**
 * Client contract 1.60 (ADR-0044 Amendment 1): a refused or narrowed
 * delegation, live (`delegation_refused` and `delegation_narrowed` frames) and
 * after a reload (`meta.delegation_notices_v1`), folds into the same
 * `delegationNotices`.
 */
import { describe, expect, it } from 'vitest';

import type { MessageGroupWire } from '@/entities/message/lib/wire';
import { socketMessageSchema } from '@/shared/api/socket/messages';

import { applyChatStreamFrame, type ChatStreamContext } from './chatStreamReducer';
import { resetTurnForReplay } from './chatStreamReattach';
import { recordStreamFailure } from './chatStreamSettle';
import { HANDLED_STREAM_TYPES } from './chatStreamFrame';
import { convertMessagesToChatHistory, type ChatMessage } from './convertMessagesToChatHistory';

const MESSAGE_ID = '63c6d989-2860-5d68-9e3e-3587c63350d3';
const CONTEXT: ChatStreamContext = { name: 'Agent', now: () => '2026-11-10T00:00:00.000Z' };
const pending: ChatMessage = { id: MESSAGE_ID, role: 'assistant', name: 'Agent', content: '', createdAt: '2026-11-10T00:00:00.000Z', isStreaming: true, isLoading: true };

const refused = {
  id: 'call-1',
  kind: 'refused',
  reason: 'no_grant',
  code: 'DELEGATION_GRANT_MISSING',
  edge: { caller_id: 2, caller_name: 'Agent2', callee_id: 1, callee_name: 'Agent1', kind: 'agent_tool' },
  missing: [{ kind: 'toolkit', name: 'Git' }],
  can_review: true,
};
const narrowed = { ...refused, id: 'call-2', kind: 'narrowed', reason: 'stale', code: undefined, missing: [{ kind: 'toolkit', name: 'Database' }] };

const frame = (type: string, notice: unknown) => ({ type, message_id: MESSAGE_ID, response_metadata: { delegation_v1: notice } }) as const;

describe('delegation notices on the live stream', () => {
  it('are handled, schema-valid frames', () => {
    for (const type of ['delegation_refused', 'delegation_narrowed']) {
      expect(HANDLED_STREAM_TYPES.has(type)).toBe(true);
      expect(socketMessageSchema.safeParse(frame(type, refused)).success).toBe(true);
    }
  });

  it('fold into the answer, never into its text or tool actions', () => {
    let history: readonly ChatMessage[] = [pending];
    history = applyChatStreamFrame(history, frame('delegation_refused', refused), CONTEXT);
    history = applyChatStreamFrame(history, frame('delegation_narrowed', narrowed), CONTEXT);
    const answer = history[0];
    expect(answer?.content).toBe('');
    expect(answer?.toolActions).toBeUndefined();
    expect(answer?.delegationNotices?.map((notice) => [notice.id, notice.kind, notice.reason, notice.calleeName, notice.canReview])).toEqual([
      ['call-1', 'refused', 'no_grant', 'Agent1', true],
      ['call-2', 'narrowed', 'stale', 'Agent1', true],
    ]);
  });

  it('keep one notice per call when a frame is replayed', () => {
    const once = applyChatStreamFrame([pending], frame('delegation_refused', refused), CONTEXT);
    const twice = applyChatStreamFrame(once, frame('delegation_refused', refused), CONTEXT);
    expect(twice[0]?.delegationNotices).toHaveLength(1);
  });

  it('keep the same history for a frame that is not a notice', () => {
    const history = [pending];
    expect(applyChatStreamFrame(history, frame('delegation_refused', { id: 'x' }), CONTEXT)).toBe(history);
  });

  it('a replay from cursor 0 rebuilds the notices once', () => {
    const seeded = applyChatStreamFrame([pending], frame('delegation_refused', refused), CONTEXT);
    let replayed = resetTurnForReplay(seeded, MESSAGE_ID);
    expect(replayed[0]?.delegationNotices).toBeUndefined();
    replayed = applyChatStreamFrame(replayed, frame('delegation_refused', refused), CONTEXT);
    expect(replayed[0]?.delegationNotices).toEqual(seeded[0]?.delegationNotices);
  });

  it.each([
    [
      'Agent node',
      {
        id: 'pipeline_node:delegate',
        kind: 'refused',
        reason: 'no_grant',
        code: 'DELEGATION_GRANT_MISSING',
        edge: { caller_id: 174, caller_name: 'P0r refusal', callee_id: 171, callee_name: 'delegate', kind: 'pipeline_node' },
        missing: [{ kind: 'toolkit', name: 'c1mockmcp' }],
        can_review: true,
      },
    ],
    [
      'saved-pipeline node (#2235)',
      {
        id: 'pipeline_node:child_pipe',
        kind: 'refused',
        reason: 'no_grant',
        code: 'DELEGATION_GRANT_MISSING',
        edge: { caller_id: 174, caller_name: 'P0r refusal', callee_id: 172, callee_name: 'child_pipe', kind: 'pipeline_node' },
        missing: [{ kind: 'toolkit', name: 'c1mockmcp' }],
        can_review: true,
      },
    ],
  ])('FU-360: assembly-time delegation_refused for a %s creates the answer live before execution.failed and matches reload', (_label, nodeNotice) => {
    const question: ChatMessage = {
      id: 'q-1',
      questionId: 'q-1',
      role: 'user',
      name: 'Alice',
      content: 'run pipeline',
      createdAt: '2026-11-10T00:00:00.000Z',
    };
    const liveFrame = {
      type: 'delegation_refused',
      message_id: MESSAGE_ID,
      question_id: 'q-1',
      execution_generation: 'q-1',
      response_metadata: { delegation_v1: nodeNotice },
    } as const;
    const afterNotice = applyChatStreamFrame([question], liveFrame, CONTEXT);
    expect(afterNotice).toHaveLength(2);
    expect(afterNotice[1]?.id).toBe(MESSAGE_ID);
    expect(afterNotice[1]?.delegationNotices).toHaveLength(1);

    const settled = recordStreamFailure(afterNotice, 'Execution authorization failed.', CONTEXT, 'q-1', MESSAGE_ID, 'AUTHORIZATION_FAILED');
    expect(settled).toHaveLength(2);
    expect(settled[1]?.isStreaming).toBe(false);
    expect(settled[1]?.failureCode).toBe('AUTHORIZATION_FAILED');
    expect(settled[1]?.delegationNotices).toHaveLength(1);

    const reloaded = convertMessagesToChatHistory(
      [
        {
          id: '4',
          uuid: MESSAGE_ID,
          role: 'assistant',
          content: '',
          created_at: '2026-11-10 10:00:05',
          is_streaming: false,
          meta: {
            delegation_notices_v1: [nodeNotice],
            is_error: true,
            error: 'Execution authorization failed.',
            error_code: 'AUTHORIZATION_FAILED',
          },
        } as unknown as MessageGroupWire,
      ],
      [],
    );
    expect(settled[1]?.delegationNotices).toEqual(reloaded[0]?.delegationNotices);
  });

  it('does not create an assistant message for a malformed delegation_refused frame before agent_start', () => {
    const question: ChatMessage = {
      id: 'q-1',
      role: 'user',
      name: 'Alice',
      content: 'hi',
      createdAt: '2026-11-10T00:00:00.000Z',
    };
    const history = [question];
    expect(applyChatStreamFrame(history, frame('delegation_refused', { id: 'broken' }), CONTEXT)).toBe(history);
  });
});

describe('meta.delegation_notices_v1 after a reload', () => {
  const row = (meta: Record<string, unknown>): MessageGroupWire =>
    ({ id: '4', uuid: MESSAGE_ID, role: 'assistant', content: 'ok', created_at: '2026-11-10 10:00:05', is_streaming: false, meta }) as unknown as MessageGroupWire;

  it('reads the stored notices with the viewer stamp', () => {
    const [answer] = convertMessagesToChatHistory([row({ delegation_notices_v1: [refused, { ...narrowed, can_review: false, editors: ['Ada'] }] })], []);
    expect(answer?.delegationNotices?.map((notice) => [notice.id, notice.canReview, notice.editors])).toEqual([
      ['call-1', true, []],
      ['call-2', false, ['Ada']],
    ]);
  });

  it('an answer without notices has none', () => {
    const [answer] = convertMessagesToChatHistory([row({})], []);
    expect(answer?.delegationNotices).toBeUndefined();
  });
});
