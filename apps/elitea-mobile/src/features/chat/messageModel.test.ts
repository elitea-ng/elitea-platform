import { agentParticipant, agentsCanJoin, chatTarget, groupText, participantName, participantUserId, toChatMessages } from '@/features/chat/messageModel';

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
    expect(chatTarget({ participants })).toMatchObject({ ok: true, kind: 'agent', participantId: 2, name: 'Helper' });
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

  // A9: the target matrix.
  const agent = (id: number, name: string, kind = 'application') => ({
    id,
    entity_name: kind,
    entity_meta: { id: id * 10, project_id: 7 },
    entity_settings: { version_id: id * 100 },
    meta: { name },
  });
  const toolkit = { id: 9, entity_name: 'toolkit', entity_meta: { id: 90, project_id: 7 }, meta: { name: 'github' } };

  it('several agents: the picked one, else the one that answered last, else the first', () => {
    const detail = { participants: [user, agent(2, 'Research'), agent(3, 'Writer'), agent(6, 'Flow', 'pipeline')] };
    expect(chatTarget(detail)).toMatchObject({ ok: true, kind: 'agent', participantId: 2 });
    expect(chatTarget(detail, { lastAnsweredBy: 3 })).toMatchObject({ participantId: 3, name: 'Writer' });
    expect(chatTarget(detail, { targetParticipantId: 6, lastAnsweredBy: 3 })).toMatchObject({
      participantId: 6,
      agent: { agentId: 60, agentProjectId: 7, versionId: 600 },
    });
    // A picked agent that was removed meanwhile falls back.
    expect(chatTarget(detail, { targetParticipantId: 99 })).toMatchObject({ participantId: 2 });
    const target = chatTarget(detail);
    expect(target.ok && target.kind === 'agent' && target.choices.map((c) => c.participantId)).toEqual([2, 3, 6]);
  });

  it('toolkits next to an agent stay read-only, with the reason', () => {
    expect(chatTarget({ participants: [user, agent(2, 'Research'), toolkit] })).toEqual({ ok: false, reason: 'toolkit_agent' });
  });

  it('toolkits in a model chat are fine: the model gets them as tools', () => {
    expect(chatTarget({ participants: [user, toolkit, dummy({ model_name: 'gpt' })] })).toMatchObject({
      ok: true,
      kind: 'model',
      participantId: 4,
    });
    expect(chatTarget({ participants: [user, toolkit] })).toMatchObject({ ok: true, kind: 'model', participantId: 0, model: null });
  });

  it('refuses several models and kinds it cannot address', () => {
    expect(chatTarget({ participants: [user, dummy({ model_name: 'a' }), { ...dummy({ model_name: 'b' }), id: 5 }] })).toEqual({
      ok: false,
      reason: 'several',
    });
    expect(chatTarget({ participants: [user, { id: 8, entity_name: 'llm', entity_meta: { model_name: 'x' } }] })).toEqual({
      ok: false,
      reason: 'none',
    });
    expect(chatTarget(undefined)).toEqual({ ok: false, reason: 'none' });
  });
});

describe('authorship in a shared conversation (A2)', () => {
  // The getConversation participant shape: a user's display name is
  // meta.user_name and entity_meta.id is the USER id (not the row id).
  const shared = [
    { id: 1, entity_name: 'user', entity_meta: { id: 9 }, meta: { user_name: 'Alice Smith' } },
    { id: 4, entity_name: 'user', entity_meta: { id: 42 }, meta: { user_name: 'Bob' } },
    { id: 2, entity_name: 'application', entity_meta: { id: 5, project_id: 7 }, meta: { name: 'Helper' } },
  ];
  const groups = [
    { id: 10, uuid: 'q-alice', author_participant_id: 1, created_at: '2026-10-04 12:00:00', message_items: [text(0, 'Hello')] },
    { id: 11, uuid: 'q-bob', author_participant_id: 4, created_at: '2026-10-04 12:00:01', message_items: [text(0, 'Hi Alice')] },
    { id: 12, uuid: 'r-1', author_participant_id: 2, created_at: '2026-10-04 12:00:02', message_items: [text(0, 'Hi both')] },
  ];

  it('draws only my messages as mine, and names the other person', () => {
    const messages = toChatMessages({ participants: shared, message_groups: groups }, { selfUserId: 9 });
    expect(messages.map((m) => [m.key, m.role, m.mine, m.author])).toEqual([
      ['q-alice', 'user', true, null],
      ['q-bob', 'user', false, 'Bob'],
      ['r-1', 'assistant', false, 'Helper'],
    ]);
  });

  it('is the same transcript seen by Bob', () => {
    const messages = toChatMessages({ participants: shared, message_groups: groups }, { selfUserId: 42 });
    expect(messages.filter((m) => m.role === 'user').map((m) => [m.mine, m.author])).toEqual([
      [false, 'Alice Smith'],
      [true, null],
    ]);
  });

  it('with "me" unknown (no "who am I" answer yet), every user message is drawn mine but named', () => {
    const messages = toChatMessages({ participants: shared, message_groups: groups }, { selfUserId: null });
    expect(messages.filter((m) => m.role === 'user').map((m) => [m.mine, m.author])).toEqual([
      [true, 'Alice Smith'],
      [true, 'Bob'],
    ]);
  });

  it('with one person in the conversation every user message is mine, unnamed', () => {
    const messages = toChatMessages({ participants: participants, message_groups: [groups[0]].map((g) => ({ ...g, author_participant_id: 1 })) });
    expect(messages[0]).toMatchObject({ mine: true, author: null });
  });

  it('reads a user participant name from meta.user_name and its user id from entity_meta.id', () => {
    expect(participantName(shared[0] as never)).toBe('Alice Smith');
    expect(participantUserId(shared[1] as never)).toBe(42);
    expect(participantUserId(shared[2] as never)).toBeNull();
  });
});

describe('failed answers', () => {
  // As the 4.2.0 server stores a turn that failed: no text, the reason in the group's meta.
  const failed = {
    id: 38,
    uuid: 'f',
    author_participant_id: 2,
    content: '',
    is_streaming: false,
    created_at: '2026-10-05 17:31:52.933102',
    message_items: [],
    meta: {
      error: 'The run stopped because it reached a platform processing limit.',
      error_code: 'RESOURCE_EXHAUSTED',
      is_error: true,
    },
  };

  it('carry the stored reason, so the bubble is not blank after a reload', () => {
    const [message] = toChatMessages({ participants, message_groups: [failed] });
    expect(message).toMatchObject({ role: 'assistant', text: '', error: 'The run stopped because it reached a platform processing limit.' });
  });

  it('read the reason from `metadata` too (the messages route), and fall back to a generic one', () => {
    const [fromMetadata] = toChatMessages({
      participants,
      message_groups: [{ ...failed, meta: undefined, metadata: { is_error: true, error: 'Boom.' } }],
    });
    expect(fromMetadata.error).toBe('Boom.');
    const [bare] = toChatMessages({ participants, message_groups: [{ ...failed, meta: { is_error: true } }] });
    expect(bare.error).toBe('This answer failed.');
    const [fine] = toChatMessages({ participants, message_groups: [{ ...failed, meta: {}, message_items: [text(0, 'ok')] }] });
    expect(fine.error).toBeNull();
  });
});

describe('agentsCanJoin', () => {
  const user = { id: 1, entity_name: 'user', entity_meta: { id: 7 } };
  const dummy = { id: 4, entity_name: 'dummy', entity_meta: {}, entity_settings: {}, meta: {} };
  const toolkit = { id: 5, entity_name: 'toolkit', entity_meta: { id: 31 }, entity_settings: {}, meta: { name: 'Jira' } };

  it('a model chat without toolkits can take an agent', () => {
    expect(agentsCanJoin({ participants: [user, dummy] })).toBe(true);
  });

  it('a model chat with a toolkit cannot: adding one would lock the conversation (toolkit next to agent)', () => {
    const detail = { participants: [user, dummy, toolkit] };
    expect(agentsCanJoin(detail)).toBe(false);
    // What adding the agent would have led to:
    const withAgent = { participants: [...detail.participants, { id: 6, entity_name: 'application', entity_meta: { id: 9, project_id: 7 }, entity_settings: { version_id: 3 }, meta: { name: 'Helper' } }] };
    expect(chatTarget(withAgent)).toEqual({ ok: false, reason: 'toolkit_agent' });
  });

  it('no conversation read yet: no', () => {
    expect(agentsCanJoin(undefined)).toBe(false);
  });
});
