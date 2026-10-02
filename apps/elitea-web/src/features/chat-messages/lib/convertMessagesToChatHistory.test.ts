import { describe, expect, it } from 'vitest';

import type { MessageGroupWire, MessageParticipantWire } from '@/entities/message/lib/wire';

import { convertMessagesToChatHistory, convertToPlayerQuestion, isUserMessage } from './convertMessagesToChatHistory';

describe('isUserMessage', () => {
  it("trusts the row's own role over the relationship heuristic", () => {
    // The messages LIST endpoint — what a conversation deep link reads —
    // answers rows shaped `{id, uid, role, content}` with none of the
    // relationship fields. Without consulting `role`, the
    // `(!sentToId && !replyToId)` clause below is TRUE for an ASSISTANT reply,
    // so a reload rendered the answer as a second question authored by
    // "User No Longer Available" (measured against a live stack, #294).
    expect(isUserMessage(undefined, undefined, [], undefined, undefined, 'assistant')).toBe(false);
    expect(isUserMessage(undefined, undefined, [], undefined, undefined, 'user')).toBe(true);
  });

  it('lets an explicit role win even when the heuristic disagrees', () => {
    // A row can carry BOTH a role and relationship fields. The role is the
    // source's own statement about the message; inference is the fallback.
    expect(isUserMessage('u1', 'p2', ['u1'], undefined, undefined, 'assistant')).toBe(false);
  });

  it('recognises a roster id the row spells the other way round', () => {
    // The membership tests are strict `includes`. `sentToId` is set in every
    // case below, so the `(!sentToId && !replyToId)` clause cannot rescue a
    // missed match — the row is classified as an assistant answer instead.
    expect.soft(isUserMessage(1, 2, ['1'], undefined, undefined)).toBe(true);
    expect.soft(isUserMessage('1', 2, [1], undefined, undefined)).toBe(true);
    expect.soft(isUserMessage(9, 1, ['1'], undefined, undefined)).toBe(true);
    // ...and it must not start matching rows it never matched: an id no
    // roster entry carries is still not a user message.
    expect.soft(isUserMessage(9, 8, ['1'], undefined, undefined)).toBe(false);
  });

  it('falls back to the heuristic when no role is stated', () => {
    // The `message_group` shape carries no `role`, and that path must keep
    // behaving exactly as it did — it is what the conversation-load path uses.
    expect(isUserMessage(undefined, undefined, [], undefined, undefined)).toBe(true);
    expect(isUserMessage('p1', 'p2', [], 'r1', undefined)).toBe(false);
    expect(isUserMessage('u1', 'p2', ['u1'], 'r1', undefined)).toBe(true);
    expect(isUserMessage('p1', 'p2', [], 'r1', { entity_name: 'users' })).toBe(true);
  });
});

/*
 * ── the two id spellings that meet in this file ────────────────────────────
 *
 * `MessageGroupWire` declares `author_participant_id`/`sent_to_id` as
 * `string | number` because both spellings are wire truth: the Go payloads
 * state participant ids as NUMBERS, the socket-era payloads carried STRINGS.
 * Every lookup below compares through `isParticipant`
 * (`entities/message/lib/normalise`) for that reason; a strict `===` resolves
 * nobody the moment the two spellings cross, and resolves nobody SILENTLY.
 */

/** A user + an agent, ids spelled as strings — the socket-era roster shape. */
const SPELLED_ROSTER: readonly MessageParticipantWire[] = [
  { id: '1', meta: { user_name: 'Alice', user_avatar: 'alice.png' }, entity_meta: { id: 6 } },
  { id: '2', meta: { user_name: 'Support Agent' } },
];

/**
 * The same roster with NUMERIC ids — the shape the Go participants payload
 * actually produces, which `MessageParticipantWire.id: string` does not
 * describe (hence the cast).
 */
const NUMBERED_ROSTER = [
  { id: 1, meta: { user_name: 'Alice', user_avatar: 'alice.png' }, entity_meta: { id: 6 } },
  { id: 2, meta: { user_name: 'Support Agent' } },
] as unknown as readonly MessageParticipantWire[];

const PLAYER: { user: { name: string; avatar: string } } = {
  user: { name: 'Player', avatar: 'player.png' },
};

function question(fields: Partial<MessageGroupWire>): MessageGroupWire {
  return { id: 5, uuid: 'q-1', content: 'hi', created_at: '2026-01-01 12:00:00', ...fields };
}

describe('convertToPlayerQuestion across the two id spellings', () => {
  /*
   * ONE comparison per lookup, and each governs more than a label: the author
   * lookup decides `name` AND `avatar`, the `sent_to_id` lookup decides
   * `sentTo` (which is how the bubble knows who the question was addressed
   * to). `expect.soft` throughout, so a failing run shows the whole identity
   * of the row going at once rather than stopping at the first field — the
   * loss is structural, not cosmetic.
   */
  it('resolves the author and the recipient when the row numbers ids and the roster spells them', () => {
    const result = convertToPlayerQuestion(
      question({ author_participant_id: 1, sent_to_id: 2 }),
      PLAYER,
      SPELLED_ROSTER,
    );

    expect.soft(result.name).toBe('Alice');
    expect.soft(result.avatar).toBe('alice.png');
    expect.soft(result.sentTo).toBe(SPELLED_ROSTER[1]);
  });

  it('resolves the author and the recipient when the row spells ids and the roster numbers them', () => {
    const result = convertToPlayerQuestion(
      question({ author_participant_id: '1', sent_to_id: '2' }),
      PLAYER,
      NUMBERED_ROSTER,
    );

    expect.soft(result.name).toBe('Alice');
    expect.soft(result.avatar).toBe('alice.png');
    expect.soft(result.sentTo).toBe(NUMBERED_ROSTER[1]);
  });

  /*
   * The second comparison in this function: `firstUserMessage
   * .author_participant_id` (declared `string | number`) against the resolved
   * participant's `id` (declared `string`). It is the switch that decides
   * whether the player's own name and avatar are used at all, and the two
   * sides were declared in different spellings and compared strictly.
   */
  it('substitutes the player identity across either spelling of firstUserMessage', () => {
    const numbered = convertToPlayerQuestion(
      question({ author_participant_id: '1' }),
      { ...PLAYER, firstUserMessage: { author_participant_id: 1 } },
      SPELLED_ROSTER,
    );
    const spelled = convertToPlayerQuestion(
      question({ author_participant_id: 1 }),
      { ...PLAYER, firstUserMessage: { author_participant_id: '1' } },
      NUMBERED_ROSTER,
    );

    expect.soft(numbered.name).toBe('Player');
    expect.soft(numbered.avatar).toBe('player.png');
    expect.soft(spelled.name).toBe('Player');
    expect.soft(spelled.avatar).toBe('player.png');
  });

  it('does not caption an unresolved author as the player when neither side states an id', () => {
    // `undefined === undefined` was TRUE: a playerInfo carrying no
    // `firstUserMessage` matched an author who resolved to nobody, and the
    // player's name and avatar were stamped onto a row that states no author.
    const result = convertToPlayerQuestion(question({}), PLAYER, SPELLED_ROSTER);

    expect.soft(result.name).toBe('You');
    expect.soft(result.avatar).toBe('');
  });
});

describe('convertMessagesToChatHistory over a numbered-id payload', () => {
  /*
   * `isUserMessage`'s roster membership tests are the same lookup in
   * `includes` form, and `includes` is strict. A Go row stating
   * `author_participant_id: 1` against a roster of string ids matched neither
   * clause; its `sent_to_id` is set, so `(!sentToId && !replyToId)` is false
   * too — and the user's own question was routed to the ASSISTANT normaliser.
   *
   * That one comparison therefore governs the entire row: role, name, avatar,
   * the `userId` the edit and delete controls gate on, and `sentTo`. Asserted
   * softly, together, because they are lost together.
   */
  it('classifies the question as the user\'s and carries its whole identity through', () => {
    const history = convertMessagesToChatHistory(
      [question({ author_participant_id: 1, sent_to_id: 2 })],
      SPELLED_ROSTER,
    );

    expect.soft(history[0]?.role).toBe('user');
    expect.soft(history[0]?.name).toBe('Alice');
    expect.soft(history[0]?.avatar).toBe('alice.png');
    expect.soft(history[0]?.userId).toBe('6');
    expect.soft(history[0]?.sentTo).toBe(SPELLED_ROSTER[1]);
  });

  it('does the same when the row spells its ids and the roster numbers them', () => {
    const history = convertMessagesToChatHistory(
      [question({ author_participant_id: '1', sent_to_id: '2' })],
      NUMBERED_ROSTER,
    );

    expect.soft(history[0]?.role).toBe('user');
    expect.soft(history[0]?.name).toBe('Alice');
    expect.soft(history[0]?.avatar).toBe('alice.png');
    expect.soft(history[0]?.userId).toBe('6');
    expect.soft(history[0]?.sentTo).toBe(NUMBERED_ROSTER[1]);
  });
});

/**
 * The measured answer of `GET /api/v2/elitea_core/messages/prompt_lib/90106/2`
 * — a two-message conversation read back after a reload, verbatim except for
 * elided opaque values. Its shape is the whole point: `items[]` carries
 * `uid`/`role`/`metadata` and NO `author_participant_id`, no `users` array,
 * no `participants` and no `sent_to`, so there is nothing in it that names
 * the author of the user's own question.
 */
const RELOADED_TRANSCRIPT_ITEMS = [
  {
    id: '4',
    uid: '0f0a1c2d-0000-4000-8000-000000000004',
    conversation_id: '2',
    role: 'assistant',
    content: 'ELITEA_OK',
    content_type: 'text',
    metadata: {},
    created_at: '2026-08-29 10:00:05',
  },
  {
    id: '3',
    uid: '0f0a1c2d-0000-4000-8000-000000000003',
    conversation_id: '2',
    role: 'user',
    content: 'Reply with exactly: ELITEA_OK',
    content_type: 'text',
    metadata: { interaction_uuid: '9b1d0f6a-0000-4000-8000-00000000abcd' },
    created_at: '2026-08-29 10:00:00',
  },
] as const;

/**
 * The `uid`→`uuid` / `metadata`→`meta` rename `pages/chat/useChatPageData.ts`'s
 * `adaptCurrentMessageRow` performs at the API composition boundary, so this
 * fixture reaches the converter the way the real rows do. Deliberately
 * nothing else: no author field is invented, because the endpoint sends none.
 */
function adaptRow(row: (typeof RELOADED_TRANSCRIPT_ITEMS)[number]): MessageGroupWire {
  const { uid, metadata, ...rest } = row;
  return { ...rest, uuid: uid, meta: metadata } as MessageGroupWire;
}

describe('convertMessagesToChatHistory over a reloaded transcript', () => {
  const history = convertMessagesToChatHistory(RELOADED_TRANSCRIPT_ITEMS.map(adaptRow), []);

  it('retains scoped persisted trace references for lazy detail loading', () => {
    const reference = { projectId: '2', conversationId: '543', messageGroupId: 5820, steps: [{ id: 7279 }], failed: false };
    const rows = RELOADED_TRANSCRIPT_ITEMS.map(adaptRow).map(row => ({ ...row, persisted_trace: reference }));
    const answer = convertMessagesToChatHistory(rows, []).find(message => message.role === 'assistant');
    expect(answer?.persistedTrace).toEqual(reference);
  });

  it('does not caption the reader\'s own question as a departed user', () => {
    const question = history.find((message) => message.role === 'user');
    expect(question?.content).toBe('Reply with exactly: ELITEA_OK');
    // Empty, not "User No Longer Available": the row states no author, so the
    // renderer (`ui/chat-box/UserMessage.tsx`) substitutes the signed-in user.
    expect(question?.name).toBe('');
  });

  it('sorts the rows chronologically even though the endpoint answers newest-first', () => {
    expect(history.map((message) => message.role)).toEqual(['user', 'assistant']);
  });

  it('never produces the departed-user string anywhere in the transcript', () => {
    expect(JSON.stringify(history)).not.toContain('User No Longer Available');
  });

  it('carries the answer through as an assistant message with its own content', () => {
    const answer = history.find((message) => message.role === 'assistant');
    expect(answer?.content).toBe('ELITEA_OK');
  });
});

/**
 * The reasoning split has to survive a reload, not only a live turn.
 *
 * Measured against the native Rust runtime with Qwen3.5: the STORED answer of
 * an ordinary turn opens with the model's whole monologue and carries a bare
 * `</think>` before the reply — the opening tag never appears, because the
 * provider's chat template consumes it. `chatStreamReasoning` peels that off
 * while the turn streams, so the bubble read correctly until the page was
 * reloaded and the same text came back through this converter untouched. One
 * message rendered two different ways is worse than either way alone, because
 * only one of them is ever on screen at a time.
 */
describe('convertMessagesToChatHistory over a stored reasoning answer', () => {
  const STORED_REASONING = [
    {
      id: '9',
      uid: '0f0a1c2d-0000-4000-8000-000000000009',
      conversation_id: '3',
      role: 'assistant',
      content: 'Thinking Process:\n\n1. The user asked for the capital.\n</think>\n\nThe capital of Japan is Tokyo.',
      content_type: 'text',
      metadata: {},
      created_at: '2026-08-29 10:00:05',
    },
  ] as const;

  const history = convertMessagesToChatHistory(
    STORED_REASONING.map((row) => {
      const { uid, metadata, ...rest } = row;
      return { ...rest, uuid: uid, meta: metadata } as MessageGroupWire;
    }),
    [],
  );
  const answer = history.find((message) => message.role === 'assistant');

  it('leaves the bubble holding only the answer', () => {
    expect(answer?.content).toBe('The capital of Japan is Tokyo.');
  });

  it('keeps the monologue, in the row the live turn would have built', () => {
    const reasoning = (answer?.toolActions ?? []).find(
      (action) => (action as { id?: string }).id === `reasoning_${answer?.id ?? ''}`,
    ) as { content?: string } | undefined;
    expect(reasoning?.content).toContain('The user asked for the capital.');
  });

  it('leaves an answer with no reasoning exactly as it was', () => {
    const plain = convertMessagesToChatHistory(
      [
        {
          id: '10',
          uuid: '0f0a1c2d-0000-4000-8000-000000000010',
          conversation_id: '3',
          role: 'assistant',
          content: 'Tokyo.',
          created_at: '2026-08-29 10:00:06',
          meta: {},
        } as unknown as MessageGroupWire,
      ],
      [],
    );
    expect(plain[0]?.content).toBe('Tokyo.');
    expect(plain[0]?.toolActions ?? []).toHaveLength(0);
  });
});
