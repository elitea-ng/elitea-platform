import { describe, expect, it, vi } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';
import { ToolActionStatus } from '@/shared/lib/chat';
import { ROLES } from '@/shared/lib/enums';

import {
  buildChatContinuePayload,
  buildDeclinedServersList,
  buildDefaultMessagePayload,
  buildOptimisticUserMessage,
  buildRegeneratePayload,
  buildSendResult,
  extractCopyableContent,
  findActionRequiredToolAction,
  findQuestionForAnswer,
  findQuestionText,
  maybeSetStreamingInfo,
  readServerUrl,
  regeneratingPatch,
  resolveConversationForSend,
  resolveParticipantId,
  resolveUploadConversationId,
  toProjectIdString,
  trackMcpAuthDecision,
  UPLOAD_FAILED,
  uploadPendingAttachments,
  type ChatBoxHandlerDeps,
  type ToolActionLike,
} from './useChatBoxHandlers.helpers';

describe('toProjectIdString', () => {
  it('converts number to string', () => {
    expect(toProjectIdString(42)).toBe('42');
  });

  it('passes string through', () => {
    expect(toProjectIdString('proj-1')).toBe('proj-1');
  });

  it('converts undefined to empty string', () => {
    expect(toProjectIdString(undefined)).toBe('');
  });
});

describe('resolveParticipantId', () => {
  it('extracts id from object', () => {
    expect(resolveParticipantId({ id: 'p1' })).toBe('p1');
  });

  it('returns undefined for null', () => {
    expect(resolveParticipantId(null)).toBeUndefined();
  });
});

describe('findActionRequiredToolAction', () => {
  it('finds the action_required tool action', () => {
    const msg = { toolActions: [
      { status: 'completed', name: 'a' },
      { status: ToolActionStatus.actionRequired, name: 'b' },
    ] } as unknown as ChatMessage;
    expect(findActionRequiredToolAction(msg)?.name).toBe('b');
  });

  it('returns undefined when none match', () => {
    const msg = { toolActions: [{ status: 'done' }] } as unknown as ChatMessage;
    expect(findActionRequiredToolAction(msg)).toBeUndefined();
  });

  it('handles undefined message', () => {
    expect(findActionRequiredToolAction(undefined)).toBeUndefined();
  });
});

describe('readServerUrl', () => {
  it('extracts server_url from toolOutputs', () => {
    const action: ToolActionLike = { toolOutputs: { server_url: 'https://example.com' } };
    expect(readServerUrl(action)).toBe('https://example.com');
  });

  it('returns undefined for empty or non-string', () => {
    expect(readServerUrl({ toolOutputs: { server_url: '' } })).toBeUndefined();
    expect(readServerUrl({ toolOutputs: { server_url: 123 } })).toBeUndefined();
    expect(readServerUrl(undefined)).toBeUndefined();
  });
});

describe('trackMcpAuthDecision', () => {
  it('adds to map on decline', () => {
    const ref = { current: new Map<string, Record<string, unknown>>() };
    const action: ToolActionLike = { name: 'tool1', toolOutputs: { tool_name: 'mcp_tool' }, toolMeta: {} };
    trackMcpAuthDecision(ref, action, 'https://srv.com', true);
    expect(ref.current.has('https://srv.com')).toBe(true);
    expect(ref.current.get('https://srv.com')?.tool_name).toBe('mcp_tool');
  });

  it('removes from map on approve', () => {
    const ref = { current: new Map([['https://srv.com', { tool_name: 'x' }]]) };
    trackMcpAuthDecision(ref, undefined, 'https://srv.com', false);
    expect(ref.current.has('https://srv.com')).toBe(false);
  });

  it('no-ops when serverUrl is empty', () => {
    const ref = { current: new Map<string, Record<string, unknown>>() };
    trackMcpAuthDecision(ref, undefined, '', true);
    expect(ref.current.size).toBe(0);
  });
});

describe('findQuestionText', () => {
  const history: ChatMessage[] = [
    { id: 'q1', content: 'Hello', role: ROLES.User } as ChatMessage,
    { id: 'a1', content: 'Hi', role: ROLES.Assistant, questionId: 'q1' } as ChatMessage,
  ];

  it('finds the question text for an answer', () => {
    expect(findQuestionText(history, history[1]!)).toBe('Hello');
  });

  it('returns undefined when questionId is missing', () => {
    expect(findQuestionText(history, history[0]!)).toBeUndefined();
  });
});

describe('buildDefaultMessagePayload', () => {
  it('builds minimal payload', () => {
    const result = buildDefaultMessagePayload({
      question: 'hi', questionId: 'q1', participant: { id: 'p1' }, conversationUuid: 'conv-1',
    });
    expect(result.question).toBe('hi');
    expect(result.question_id).toBe('q1');
    expect(result.conversation_uuid).toBe('conv-1');
    expect(result.participant_id).toBe('p1');
  });

  it('omits participant_id when undefined', () => {
    const result = buildDefaultMessagePayload({ question: 'x', questionId: 'q', participant: null });
    expect('participant_id' in result).toBe(false);
  });

  it('includes attachments when non-empty', () => {
    const result = buildDefaultMessagePayload({
      question: 'x', questionId: 'q', participant: null, attachmentList: [{ name: 'f.txt' }],
    });
    expect(result.attachments).toHaveLength(1);
  });

  it('includes user routing when isSendingToUser', () => {
    const result = buildDefaultMessagePayload({
      question: 'x', questionId: 'q', participant: null, isSendingToUser: true, userIds: ['u1'],
    });
    expect(result.isSendingToUser).toBe(true);
    expect(result.userIds).toEqual(['u1']);
  });
});

describe('buildChatContinuePayload', () => {
  it('builds continue payload from deps and params', () => {
    const deps = { conversationUuid: 'conv-1', projectId: 42 } as ChatBoxHandlerDeps;
    const result = buildChatContinuePayload(deps, { messageId: 'm1', threadId: 't1', question: 'go on' });
    expect(result.conversation_uuid).toBe('conv-1');
    expect(result.project_id).toBe('42');
    expect(result.message_id).toBe('m1');
    expect(result.thread_id).toBe('t1');
    expect(result.user_input).toBe('go on');
  });
});

describe('buildDeclinedServersList', () => {
  it('returns empty for undefined ref', () => {
    expect(buildDeclinedServersList(undefined)).toEqual([]);
  });

  it('maps entries with actual_server_url fallback', () => {
    const ref = { current: new Map([['https://fallback.com', { actual_server_url: 'https://real.com', tool_name: 't' }]]) };
    const list = buildDeclinedServersList(ref);
    expect(list[0]).toEqual(expect.objectContaining({ server_url: 'https://real.com', tool_name: 't' }));
  });

  it('uses map key when actual_server_url missing', () => {
    const ref = { current: new Map([['https://key.com', { tool_name: 't' }]]) };
    const list = buildDeclinedServersList(ref);
    expect(list[0]).toEqual(expect.objectContaining({ server_url: 'https://key.com' }));
  });
});

describe('extractCopyableContent', () => {
  it('joins messageItems text content', () => {
    const msg = {
      messageItems: [
        { item_type: 'text_message', item_details: { content: 'hello' } },
        { item_type: 'text_message', item_details: { content: 'world' } },
      ],
    } as unknown as ChatMessage;
    expect(extractCopyableContent(msg)).toBe('hello, world');
  });

  it('handles canvas_message type', () => {
    const msg = {
      messageItems: [{ item_type: 'canvas_message', item_details: { latest_version: { canvas_content: 'code' } } }],
    } as unknown as ChatMessage;
    expect(extractCopyableContent(msg)).toBe('code');
  });

  it('handles attachment_message type', () => {
    const msg = {
      messageItems: [{ item_type: 'attachment_message', item_details: { name: 'file.pdf' } }],
    } as unknown as ChatMessage;
    expect(extractCopyableContent(msg)).toBe('[file.pdf]');
  });

  it('falls back to content when no messageItems', () => {
    const msg = { content: 'fallback text', messageItems: [] } as unknown as ChatMessage;
    expect(extractCopyableContent(msg)).toBe('fallback text');
  });
});

describe('resolveConversationForSend', () => {
  it('returns existing uuid when deps has one', async () => {
    const deps = { conversationUuid: 'existing' } as ChatBoxHandlerDeps;
    const result = await resolveConversationForSend(deps, 'q');
    expect(result.uuid).toBe('existing');
    expect(result.createdConversation).toBeUndefined();
  });

  it('creates conversation when uuid absent', async () => {
    const deps = { createConversation: vi.fn().mockResolvedValue({ id: 1, uuid: 'new-uuid' }) } as unknown as ChatBoxHandlerDeps;
    const result = await resolveConversationForSend(deps, 'hello');
    expect(result.uuid).toBe('new-uuid');
    expect(result.createdConversation).toEqual({ id: 1, uuid: 'new-uuid' });
  });

  it('returns empty when createConversation is not provided', async () => {
    const deps = {} as ChatBoxHandlerDeps;
    const result = await resolveConversationForSend(deps, 'q');
    expect(result).toEqual({});
  });
});

describe('uploadPendingAttachments', () => {
  it('returns attachments as-is when empty', async () => {
    const deps = {} as ChatBoxHandlerDeps;
    expect(await uploadPendingAttachments(deps, [], 'c1')).toEqual([]);
  });

  it('returns attachments when no uploadAttachments fn', async () => {
    const files = [new File(['x'], 'x.txt')];
    const deps = {} as ChatBoxHandlerDeps;
    expect(await uploadPendingAttachments(deps, files, 'c1')).toBe(files);
  });

  it('returns UPLOAD_FAILED on failure', async () => {
    const deps = { uploadAttachments: vi.fn().mockResolvedValue({ success: false, uploaded: [] }) } as unknown as ChatBoxHandlerDeps;
    const result = await uploadPendingAttachments(deps, [new File(['x'], 'x.txt')], 'c1');
    expect(result).toBe(UPLOAD_FAILED);
  });

  it('maps uploaded outcomes on success', async () => {
    const deps = {
      uploadAttachments: vi.fn().mockResolvedValue({ success: true, uploaded: [{ filepath: '/f', sanitizedName: 'clean.txt' }] }),
    } as unknown as ChatBoxHandlerDeps;
    const result = await uploadPendingAttachments(deps, [new File(['x'], 'x.txt')], 'c1');
    expect(result).toEqual([{ filepath: '/f', name: 'clean.txt' }]);
  });
});

describe('buildOptimisticUserMessage', () => {
  it('builds a user message with all fields', () => {
    const msg = buildOptimisticUserMessage('q1', 'Hello', { id: 'u1', name: 'Alice', avatar: 'a.png' }, 'p1');
    expect(msg.id).toBe('q1');
    expect(msg.role).toBe(ROLES.User);
    expect(msg.name).toBe('Alice');
    expect(msg.content).toBe('Hello');
    expect(msg.userId).toBe('u1');
    expect(msg.participantId).toBe('p1');
  });

  it('omits optional fields when not provided', () => {
    const msg = buildOptimisticUserMessage('q1', 'Hi', undefined, undefined);
    expect(msg.name).toBe('');
    expect('avatar' in msg).toBe(false);
    expect('userId' in msg).toBe(false);
    expect('participantId' in msg).toBe(false);
  });
});

/**
 * The identifier an attachment is uploaded under decides whether the turn it
 * rides can be admitted at all: the object is keyed `{this}/{filename}` and
 * admission refuses any name not prefixed by the conversation's UUID. Both
 * branches are asserted as "is the uuid" AND "is not the id", because the
 * defect this replaced was a value that looked perfectly valid.
 */
describe('resolveUploadConversationId', () => {
  it('returns the CREATED conversation’s uuid, not its numeric id', () => {
    expect(resolveUploadConversationId({ id: 99, uuid: 'created-uuid' }, 'existing-uuid')).toBe('created-uuid');
    expect(resolveUploadConversationId({ id: 99, uuid: 'created-uuid' }, 'existing-uuid')).not.toBe(99);
  });

  it('returns the EXISTING conversation’s uuid when the send created none', () => {
    expect(resolveUploadConversationId(undefined, 'existing-uuid')).toBe('existing-uuid');
  });

  /**
   * A created conversation that answered without a uuid must not silently
   * fall back to the id — there is nothing to key by, and uploading to a
   * wrong key stores bytes no turn can ever use.
   */
  it('is undefined when neither conversation states a uuid', () => {
    expect(resolveUploadConversationId({ id: 99 }, undefined)).toBeUndefined();
  });
});

describe('buildSendResult', () => {
  it('includes createdConversation when present', () => {
    expect(buildSendResult({ id: 1, uuid: 'u' })).toEqual({ success: true, createdConversation: { id: 1, uuid: 'u' } });
  });

  it('carries the created conversation on a FAILED result too — the row is committed before the transport is tried', () => {
    expect(buildSendResult({ id: 1, uuid: 'u' }, false)).toEqual({ success: false, createdConversation: { id: 1, uuid: 'u' } });
  });

  it('returns success only when no conversation created', () => {
    expect(buildSendResult(undefined)).toEqual({ success: true });
  });
});

describe('findQuestionForAnswer', () => {
  it('finds the question message', () => {
    const h = [{ id: 'q1', content: 'Q' } as ChatMessage, { id: 'a1', questionId: 'q1' } as ChatMessage];
    expect(findQuestionForAnswer(h, h[1])?.content).toBe('Q');
  });

  it('returns undefined for undefined answer', () => {
    expect(findQuestionForAnswer([], undefined)).toBeUndefined();
  });
});

describe('regeneratingPatch', () => {
  it('clears content and marks as loading', () => {
    const item = { id: 'a1', content: 'old', toolActions: [{}], references: ['r'] } as unknown as ChatMessage;
    const patched = regeneratingPatch(item);
    expect(patched.content).toBe('');
    expect(patched.isLoading).toBe(true);
    expect(patched.isStreaming).toBe(true);
    expect(patched.toolActions).toEqual([]);
    expect(patched.references).toEqual([]);
  });
});

describe('buildRegeneratePayload', () => {
  it('builds payload with socket id and updated items', () => {
    const deps = { projectId: 5, conversationUuid: 'conv', socketId: 's1' } as unknown as ChatBoxHandlerDeps;
    const question = { id: 'q1', content: 'Why?' } as ChatMessage;
    const result = buildRegeneratePayload(deps, 'msg1', question, [{ content: 'edited', item_type: 'text' }]);
    expect(result.projectId).toBe('5');
    expect(result.message_id).toBe('msg1');
    expect(result.question).toBe('Why?');
    expect(result.sid).toBe('s1');
    expect(result.updated_items).toHaveLength(1);
  });
});

describe('maybeSetStreamingInfo', () => {
  it('calls setter when id is defined', () => {
    const setter = vi.fn();
    maybeSetStreamingInfo(setter, 'abc');
    expect(setter).toHaveBeenCalledWith('abc');
  });

  it('does not call setter when id is undefined', () => {
    const setter = vi.fn();
    maybeSetStreamingInfo(setter, undefined);
    expect(setter).not.toHaveBeenCalled();
  });
});
