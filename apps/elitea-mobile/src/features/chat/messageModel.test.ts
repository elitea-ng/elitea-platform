import { agentParticipant, chatTarget, groupText, toChatMessages } from '@/features/chat/messageModel';

const participants = [
  { id: 1, entity_name: 'user', entity_meta: { id: 9 } },
  { id: 2, entity_name: 'application', entity_meta: { id: 5 }, meta: { name: 'Helper' } },
];

const text = (order: number, content: string) => ({
  id: order + 100,
  item_type: 'text_message',
  order_index: order,
  item_details: { content },
});

describe('toChatMessages', () => {
  it('maps roles through participants and sorts oldest first', () => {
    const messages = toChatMessages({
      participants,
      message_groups: [
        { id: 11, uuid: 'b', author_participant_id: 2, is_streaming: false, created_at: '2026-10-04 12:00:01.5', message_items: [text(0, 'Hi!')] },
        { id: 10, uuid: 'a', author_participant_id: 1, is_streaming: false, created_at: '2026-10-04 12:00:00', message_items: [text(0, 'Hello')] },
      ],
    });
    expect(messages.map((m) => [m.key, m.role, m.author, m.text])).toEqual([
      ['a', 'user', null, 'Hello'],
      ['b', 'assistant', 'Helper', 'Hi!'],
    ]);
  });

  it('orders text items by order_index and skips unknown item types', () => {
    const { text: body, unsupported } = groupText({
      message_items: [
        text(2, 'third'),
        { id: 1, item_type: 'attachment_message', order_index: 1, item_details: { name: 'x.pdf' } },
        text(0, 'first'),
        { id: 2, item_type: 'brand_new_type', order_index: 3 },
      ],
    });
    expect(body).toBe('first\n\nthird');
    expect(unsupported).toBe(true);
  });

  it('falls back to the group content projection when there are no items', () => {
    expect(groupText({ content: 'projected', message_items: [] }).text).toBe('projected');
  });

  it('is defensive about malformed groups and flags streaming ones', () => {
    const messages = toChatMessages({
      participants,
      message_groups: [
        null as never,
        { nonsense: true } as never,
        { id: 12, uuid: 'c', author_participant_id: 99, is_streaming: true, created_at: 'garbage', message_items: 'nope' as never },
      ],
    });
    expect(messages).toHaveLength(1);
    expect(messages[0]).toMatchObject({ key: 'c', role: 'assistant', author: null, text: '', isStreaming: true, createdAt: null });
    expect(toChatMessages(undefined)).toEqual([]);
  });
});

describe('agentParticipant', () => {
  it('finds the single application participant', () => {
    expect(agentParticipant({ participants })).toEqual({ ok: true, participantId: 2, name: 'Helper' });
  });

  it('refuses none or several', () => {
    expect(agentParticipant({ participants: [participants[0]] })).toEqual({ ok: false, reason: 'none' });
    expect(
      agentParticipant({ participants: [...participants, { id: 3, entity_name: 'application' }] }),
    ).toEqual({ ok: false, reason: 'several' });
    expect(agentParticipant(undefined)).toEqual({ ok: false, reason: 'none' });
  });
});

// DEF-3 (Agent Zefir E2E): a conversation with no agent (a plain model chat)
// could not be messaged from mobile. It is addressed as a model turn.
describe('chatTarget', () => {
  const user = { id: 1, entity_name: 'user', entity_meta: { id: 7 } };
  const dummy = (llm: Record<string, unknown> | null) => ({
    id: 4,
    entity_name: 'dummy',
    entity_meta: {},
    entity_settings: llm === null ? {} : { llm_settings: llm },
    meta: { name: 'EliteA' },
  });

  it('addresses the single agent', () => {
    expect(chatTarget({ participants })).toEqual({ ok: true, kind: 'agent', participantId: 2, name: 'Helper' });
  });

  it("addresses a model conversation's model participant with its saved model", () => {
    expect(
      chatTarget({ participants: [user, dummy({ model_name: 'vllm/E2E-MOCK-MODEL', model_project_id: 90107, stream: true })] }),
    ).toEqual({
      ok: true,
      kind: 'model',
      participantId: 4,
      name: 'EliteA',
      model: { name: 'vllm/E2E-MOCK-MODEL', projectId: 90107 },
    });
    expect(chatTarget({ participants: [user, dummy({ model_name: 'gpt' })] })).toMatchObject({
      kind: 'model',
      model: { name: 'gpt', projectId: null },
    });
  });

  it('addresses participant 0 with no saved model when the conversation has only its user', () => {
    for (const detail of [{ participants: [user] }, { participants: [] }, { participants: [user, dummy(null)] }]) {
      expect(chatTarget(detail)).toMatchObject({ ok: true, kind: 'model', model: null });
    }
    expect(chatTarget({ participants: [user] })).toMatchObject({ participantId: 0 });
    expect(chatTarget({ participants: [user, dummy(null)] })).toMatchObject({ participantId: 4 });
  });

  it('refuses several agents, and a conversation with other participants it cannot address', () => {
    expect(chatTarget({ participants: [...participants, { id: 3, entity_name: 'application' }] })).toEqual({
      ok: false,
      reason: 'several',
    });
    expect(chatTarget({ participants: [user, { id: 9, entity_name: 'toolkit' }] })).toEqual({ ok: false, reason: 'none' });
    expect(chatTarget({ participants: [user, dummy({ model_name: 'a' }), { ...dummy({ model_name: 'b' }), id: 5 }] })).toEqual({
      ok: false,
      reason: 'several',
    });
    expect(chatTarget(undefined)).toEqual({ ok: false, reason: 'none' });
  });
});
