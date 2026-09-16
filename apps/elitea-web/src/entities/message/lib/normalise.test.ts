import { describe, expect, it } from 'vitest';

import { convertTime, isParticipant, normaliseAssistantMessage, normaliseUserMessage } from './normalise';
import type { MessageAuthorWire, MessageGroupMetaWire, MessageGroupWire, MessageParticipantWire } from './wire';

describe('convertTime', () => {
  it('converts a space-separated Postgres-style timestamp to ISO', () => {
    expect(convertTime('2026-01-01 12:30:00')).toBe('2026-01-01T12:30:00Z');
  });

  it('returns a timestamp already ending in Z unchanged', () => {
    expect(convertTime('2026-01-01T12:30:00Z')).toBe('2026-01-01T12:30:00Z');
  });

  it('returns a timestamp with a + offset unchanged', () => {
    expect(convertTime('2026-01-01T12:30:00+02:00')).toBe('2026-01-01T12:30:00+02:00');
  });

  it('appends Z to a bare timestamp with none of the above', () => {
    expect(convertTime('2026-01-01T12:30:00')).toBe('2026-01-01T12:30:00Z');
  });

  it('is idempotent for the appended-Z case, matching parseability', () => {
    const once = convertTime('2026-01-01T12:30:00');
    expect(new Date(once).toString()).not.toBe('Invalid Date');
  });
});

describe('isParticipant', () => {
  it('matches across the two id spellings that really coexist on the wire', () => {
    expect(isParticipant(7, '7')).toBe(true);
    expect(isParticipant('7', 7)).toBe(true);
    expect(isParticipant(7, 7)).toBe(true);
    expect(isParticipant('7', '7')).toBe(true);
  });

  it('does not match two different ids that merely stringify alike-ish', () => {
    expect(isParticipant(7, 70)).toBe(false);
    expect(isParticipant('7', ' 7')).toBe(false);
  });

  it('never matches on undefined, on either side', () => {
    // A row that states no id must resolve NOBODY. The trap this closes is
    // `String(undefined) === String(undefined)`, which would attribute an
    // author-less row to a participant whose id is likewise absent — and, on
    // the playback path, let a `playerInfo` with no `firstUserMessage` caption
    // an unresolved author as the player.
    expect(isParticipant(undefined, undefined)).toBe(false);
    expect(isParticipant(undefined, 7)).toBe(false);
    expect(isParticipant(7, undefined)).toBe(false);
  });
});

describe('normaliseUserMessage', () => {
  const baseGroup: MessageGroupWire = {
    id: 1,
    uuid: 'q1',
    author_participant_id: 'u1',
    content: 'hello',
    created_at: '2026-01-01 12:00:00',
  };

  it('maps the core fields, resolving the author name from meta.user_name', () => {
    const users: readonly MessageAuthorWire[] = [{ id: 'u1', meta: { user_name: 'Alice', user_avatar: 'a.png' }, entity_meta: { id: 'user-1' } }];
    const result = normaliseUserMessage(baseGroup, users, []);
    expect(result).toEqual({
      id: 'q1',
      role: 'user',
      name: 'Alice',
      avatar: 'a.png',
      content: 'hello',
      createdAt: '2026-01-01T12:00:00Z',
      userId: 'user-1',
    });
  });

  it('resolves the author across the two id spellings on the wire', () => {
    // The Go transcript endpoint states author_participant_id as a NUMBER
    // and the Go participants payload serialises participant ids as numbers
    // too; socket-era payloads carried strings. A reloaded shared
    // conversation attributes each question through exactly this lookup, so
    // every combination must resolve — a strict === across the spellings is
    // how "User No Longer Available" appeared over a user who was right
    // there in the participants array.
    const alice: MessageAuthorWire = { id: 7, meta: { user_name: 'Alice' } };
    const bob: MessageAuthorWire = { id: '8', meta: { user_name: 'Bob' } };
    const users: readonly MessageAuthorWire[] = [alice, bob];
    const numberToNumber: MessageGroupWire = { ...baseGroup, author_participant_id: 7 };
    const stringToNumber: MessageGroupWire = { ...baseGroup, author_participant_id: '7' };
    const numberToString: MessageGroupWire = { ...baseGroup, author_participant_id: 8 };
    expect(normaliseUserMessage(numberToNumber, users, []).name).toBe('Alice');
    expect(normaliseUserMessage(stringToNumber, users, []).name).toBe('Alice');
    expect(normaliseUserMessage(numberToString, users, []).name).toBe('Bob');
  });

  /*
   * The whole identity of one row, across both spellings.
   *
   * TWO lookups build it — the author one (`name`, `avatar`, `userId`) and the
   * `sent_to_id` one (`sentTo`) — and only the first was ever normalised, so a
   * Go payload numbering its ids produced a question that knew its author but
   * not who it was addressed to. `expect.soft` on every field on purpose: a
   * hard assertion stops at the first and reports the loss as one missing
   * string, which is exactly the misreading that let the asymmetry survive.
   */
  it('resolves author AND sentTo when the row numbers ids and the roster spells them', () => {
    const users: readonly MessageAuthorWire[] = [
      { id: '1', meta: { user_name: 'Alice', user_avatar: 'alice.png' }, entity_meta: { id: 6 } },
    ];
    const participants: readonly MessageParticipantWire[] = [{ id: '2', meta: { user_name: 'Support Agent' } }];
    const group: MessageGroupWire = { ...baseGroup, author_participant_id: 1, sent_to_id: 2 };

    const result = normaliseUserMessage(group, users, participants);

    expect.soft(result.name).toBe('Alice');
    expect.soft(result.avatar).toBe('alice.png');
    expect.soft(result.userId).toBe('6');
    expect.soft(result.sentTo).toBe(participants[0]);
  });

  it('resolves author AND sentTo when the row spells ids and the roster numbers them', () => {
    // The roster spelling the Go participants payload actually produces, which
    // `MessageParticipantWire.id: string` does not describe — hence the cast.
    const users: readonly MessageAuthorWire[] = [
      { id: 1, meta: { user_name: 'Alice', user_avatar: 'alice.png' }, entity_meta: { id: 6 } },
    ];
    const participants = [{ id: 2, meta: { user_name: 'Support Agent' } }] as unknown as readonly MessageParticipantWire[];
    const group: MessageGroupWire = { ...baseGroup, author_participant_id: '1', sent_to_id: '2' };

    const result = normaliseUserMessage(group, users, participants);

    expect.soft(result.name).toBe('Alice');
    expect.soft(result.avatar).toBe('alice.png');
    expect.soft(result.userId).toBe('6');
    expect.soft(result.sentTo).toBe(participants[0]);
  });

  it.each([
    { desc: 'user_name present', user: { id: 'u1', meta: { user_name: 'Alice' } }, expected: 'Alice' },
    { desc: 'no user_name, falls back to entity_meta.email', user: { id: 'u1', entity_meta: { email: 'a@x.com' } }, expected: 'a@x.com' },
    { desc: 'no name/email, falls back to entity_meta.id', user: { id: 'u1', entity_meta: { id: '42' } }, expected: 'User 42' },
    { desc: 'user has nothing usable', user: { id: 'u1' }, expected: 'User No Longer Available' },
    { desc: 'user not found at all', user: undefined, expected: 'User No Longer Available' },
  ])('resolves the author name: $desc', ({ user, expected }) => {
    const users: readonly MessageAuthorWire[] = user ? [user] : [];
    expect(normaliseUserMessage(baseGroup, users, []).name).toBe(expected);
  });

  /*
   * The reload path. `GET /elitea_core/messages/prompt_lib/90106/2` answers
   * flat rows with no `author_participant_id`, no `users` array and no
   * `sent_to` (measured against a live stack — see the fixture in
   * `features/chat-messages/lib/convertMessagesToChatHistory.test.ts`), and
   * the caption above the reader's own bubble read "User No Longer Available"
   * until the two cases below were told apart.
   */
  it('leaves the author name empty when the row states no author at all', () => {
    const group: MessageGroupWire = { id: 3, uuid: 'uid-3', content: 'hello', created_at: '2026-01-01 12:00:00' };
    expect(normaliseUserMessage(group, [], []).name).toBe('');
  });

  it('still reports a stated author that resolves to nobody as no longer available', () => {
    expect(normaliseUserMessage({ ...baseGroup, author_participant_id: 'gone' }, [], []).name).toBe(
      'User No Longer Available',
    );
  });

  it('treats an empty-string author_participant_id as no author stated', () => {
    expect(normaliseUserMessage({ ...baseGroup, author_participant_id: '' }, [], []).name).toBe('');
  });

  /*
   * Both halves of the anonymous row's identity are absent, which is the
   * whole point: an author-less row states NOTHING the renderer could
   * attribute, so `UserMessage`'s caption stays empty rather than borrowing
   * the reader's name. A `userId`-without-`name` row (which the caption used
   * to treat as "someone else, do not substitute") is a state this endpoint
   * cannot produce.
   */
  it('omits userId as well as name when the row states no author, leaving nothing to attribute', () => {
    const group: MessageGroupWire = { id: 3, uuid: 'uid-3', content: 'hello', created_at: '2026-01-01 12:00:00' };
    const result = normaliseUserMessage(group, [], []);
    expect('userId' in result).toBe(false);
    expect(result.name).toBe('');
  });

  it('omits messageItems entirely when the wire does not send message_items (not defaulted to [])', () => {
    const result = normaliseUserMessage(baseGroup, [], []);
    expect('messageItems' in result).toBe(false);
  });

  it('passes message_items through unsorted (no defaulting/sorting, unlike the assistant path)', () => {
    const group: MessageGroupWire = { ...baseGroup, message_items: [{ id: 3 }, { id: 1 }, { id: 2 }] };
    expect(normaliseUserMessage(group, [], []).messageItems).toEqual([{ id: 3 }, { id: 1 }, { id: 2 }]);
  });

  it('preserves likes: 0 rather than omitting it as falsy', () => {
    const group: MessageGroupWire = { ...baseGroup, likes: 0 };
    const result = normaliseUserMessage(group, [], []);
    expect(result.likes).toBe(0);
    expect('likes' in result).toBe(true);
  });

  it('resolves sentTo to the matched participant when sent_to_id matches one', () => {
    const group: MessageGroupWire = { ...baseGroup, sent_to_id: 'p1' };
    const participants: readonly MessageParticipantWire[] = [{ id: 'p1' }];
    expect(normaliseUserMessage(group, [], participants).sentTo).toEqual({ id: 'p1' });
  });

  it('synthesises a "User No Longer Available" sentTo when sent_to.entity_name is user but no participant matches', () => {
    const group: MessageGroupWire = { ...baseGroup, sent_to: { entity_name: 'user' } };
    expect(normaliseUserMessage(group, [], []).sentTo).toEqual({
      entity_name: 'user',
      meta: { user_name: 'User No Longer Available' },
    });
  });

  it('omits sentTo when neither a matching participant nor a user-typed sent_to exists', () => {
    const result = normaliseUserMessage(baseGroup, [], []);
    expect('sentTo' in result).toBe(false);
  });

  it('omits interactionUuid when meta is entirely absent, rather than throwing', () => {
    expect(() => normaliseUserMessage(baseGroup, [], [])).not.toThrow();
    expect('interactionUuid' in normaliseUserMessage(baseGroup, [], [])).toBe(false);
  });

  it('carries interactionUuid through from meta.interaction_uuid', () => {
    const group: MessageGroupWire = { ...baseGroup, meta: { interaction_uuid: 'iu-1' } };
    expect(normaliseUserMessage(group, [], []).interactionUuid).toBe('iu-1');
  });
});

describe('normaliseAssistantMessage', () => {
  const baseGroup: MessageGroupWire = {
    id: 2,
    uuid: 'a1',
    content: 'the answer',
    created_at: '2026-01-01 12:05:00',
  };

  it('maps the core fields for a settled (non-streaming) message', () => {
    const result = normaliseAssistantMessage(baseGroup, [], undefined);
    expect(result).toMatchObject({
      id: 'a1',
      role: 'assistant',
      content: 'the answer',
      createdAt: '2026-01-01T12:05:00Z',
      messageItems: [],
      references: [],
      toolActions: [],
      isSummarized: false,
    });
  });

  it('replaces content with the streaming placeholder while is_streaming is true', () => {
    const group: MessageGroupWire = { ...baseGroup, is_streaming: true };
    const result = normaliseAssistantMessage(group, [], undefined);
    expect(result.content).toBe('...');
    expect(result.isStreaming).toBe(true);
    expect(result.isLoading).toBe(true);
  });

  it('omits isStreaming/isLoading entirely when is_streaming is absent from the wire', () => {
    const result = normaliseAssistantMessage(baseGroup, [], undefined);
    expect('isStreaming' in result).toBe(false);
    expect('isLoading' in result).toBe(false);
  });

  it('sorts message_items by numeric id ascending, unlike the unsorted user path', () => {
    const group: MessageGroupWire = { ...baseGroup, message_items: [{ id: 3 }, { id: 1 }, { id: 2 }] };
    expect(normaliseAssistantMessage(group, [], undefined).messageItems).toEqual([{ id: 1 }, { id: 2 }, { id: 3 }]);
  });

  it('resolves questionId to the sibling question row uuid (matching that row\'s own converted id)', () => {
    const question: MessageGroupWire = { id: 10, uuid: 'q-uuid', content: 'q', created_at: '2026-01-01 12:00:00' };
    const answer: MessageGroupWire = { ...baseGroup, reply_to_id: 10 };
    const result = normaliseAssistantMessage(answer, [question, answer], undefined);
    expect(result.questionId).toBe('q-uuid');
  });

  it('falls back to the stringified numeric id when the sibling question row has no uuid', () => {
    const question: MessageGroupWire = { id: 10, uuid: '', content: 'q', created_at: '2026-01-01 12:00:00' };
    const answer: MessageGroupWire = { ...baseGroup, reply_to_id: 10 };
    expect(normaliseAssistantMessage(answer, [question, answer], undefined).questionId).toBe('10');
  });

  /*
   * MEASURED on the live stack (project 90106, conversation 471): the
   * transcript route answers the question as `{"id": "1121", ...}` and its
   * answer as `{"id": "1122", "reply_to_id": 1121}` — the SAME row id in two
   * spellings inside ONE payload, because Go serialises `Message.ID` as a
   * string (`strconv.Itoa` of the row id) and `ReplyToID` as `*int`
   * (services/elitea-main/internal/api/v2/conversations/handler.go:41,57),
   * while the other producer (`ListMessageGroups`) numbers both. A strict
   * `===` resolved NO question on the measured shape, and
   * `ChatMessageList`'s `canDeleteAiMessage` then refuses delete on every
   * answer of a reloaded conversation. `expect.soft` so one broken
   * comparison reports every spelling it governs at once.
   */
  it('links an answer to its question across the two row-id spellings', () => {
    const wireQuestion: MessageGroupWire = { id: '1121', uuid: 'q-uuid', content: 'q', created_at: '2026-08-30 01:50:58' };
    const numberedQuestion: MessageGroupWire = { ...wireQuestion, id: 1121 };
    const wireAnswer: MessageGroupWire = { ...baseGroup, id: '1122', reply_to_id: 1121 };
    const spelledAnswer: MessageGroupWire = { ...baseGroup, reply_to_id: '1121' };

    // reply_to_id: string row id <- numeric FK (the measured transcript payload)
    expect.soft(normaliseAssistantMessage(wireAnswer, [wireQuestion, wireAnswer], undefined).questionId).toBe('q-uuid');
    // reply_to_id: numeric row id <- string FK (the message-groups spelling)
    expect.soft(normaliseAssistantMessage(spelledAnswer, [numberedQuestion], undefined).questionId).toBe('q-uuid');
    // question_id, the second clause of the same lookup, in both directions
    expect.soft(normaliseAssistantMessage({ ...baseGroup, question_id: 1121 }, [wireQuestion], undefined).questionId).toBe('q-uuid');
    expect.soft(normaliseAssistantMessage({ ...baseGroup, question_id: '1121' }, [numberedQuestion], undefined).questionId).toBe('q-uuid');
  });

  /*
   * The other half of the same comparison: a strict `===` also makes
   * `undefined === undefined` TRUE, so an answer stating no reply linked
   * itself to whichever row happened to state no id.
   */
  it('links no question when neither the answer nor the candidate row states an id', () => {
    const idless = { uuid: 'x-uuid', content: 'q', created_at: '2026-01-01 12:00:00' } as unknown as MessageGroupWire;
    expect('questionId' in normaliseAssistantMessage(baseGroup, [idless], undefined)).toBe(false);
  });

  it('omits questionId when no sibling row matches reply_to_id or question_id', () => {
    const result = normaliseAssistantMessage(baseGroup, [], undefined);
    expect('questionId' in result).toBe(false);
  });

  it('populates replyToId from the raw reply_to_id FK, stringified', () => {
    const group: MessageGroupWire = { ...baseGroup, reply_to_id: 10 };
    expect(normaliseAssistantMessage(group, [], undefined).replyToId).toBe('10');
  });

  it('keeps createdAt and updatedAt as two distinct ISO strings when both are present', () => {
    const group: MessageGroupWire = { ...baseGroup, updated_at: '2026-01-01 12:10:00' };
    const result = normaliseAssistantMessage(group, [], undefined);
    expect(result.createdAt).toBe('2026-01-01T12:05:00Z');
    expect(result.updatedAt).toBe('2026-01-01T12:10:00Z');
  });

  it('omits updatedAt entirely when the wire never sent updated_at', () => {
    expect('updatedAt' in normaliseAssistantMessage(baseGroup, [], undefined)).toBe(false);
  });

  it('sets isSummarized true only when meta.context.included is exactly false', () => {
    const notIncluded: MessageGroupWire = { ...baseGroup, meta: { context: { included: false } } };
    const included: MessageGroupWire = { ...baseGroup, meta: { context: { included: true } } };
    expect(normaliseAssistantMessage(notIncluded, [], undefined).isSummarized).toBe(true);
    expect(normaliseAssistantMessage(included, [], undefined).isSummarized).toBe(false);
    expect(normaliseAssistantMessage(baseGroup, [], undefined).isSummarized).toBe(false);
  });

  it('resolves exception from meta.error when is_error is true', () => {
    const group: MessageGroupWire = { ...baseGroup, meta: { is_error: true, error: 'boom' } };
    expect(normaliseAssistantMessage(group, [], undefined).exception).toBe('boom');
  });

  it('falls back exception to content, then to the first message item, when meta.error is absent', () => {
    const group: MessageGroupWire = {
      ...baseGroup,
      meta: { is_error: true },
      message_items: [{ id: 1, item_details: { content: 'item content' } }],
    };
    const withContentOnly: MessageGroupWire = { ...baseGroup, meta: { is_error: true } };
    expect(normaliseAssistantMessage(group, [], undefined).exception).toBe('the answer');
    expect(normaliseAssistantMessage(withContentOnly, [], undefined).exception).toBe('the answer');
  });

  it('omits exception entirely when is_error is not true (not even as undefined)', () => {
    expect('exception' in normaliseAssistantMessage(baseGroup, [], undefined)).toBe(false);
  });

  it('restores the token-limit continuation signal from durable metadata', () => {
    const group: MessageGroupWire = {
      ...baseGroup,
      meta: { output_limit_reached: true, output_limit_sequence: 1, thread_id: 'thread-1' },
    };
    expect(normaliseAssistantMessage(group, [], undefined)).toMatchObject({
      threadId: 'thread-1',
      requiresConfirmation: {
        message: "Token limit reached mid-response. Press 'Continue' to see more.",
        buttonText: 'Continue',
      },
    });
  });

  it('skips an empty/whitespace-only thinking step (a real no-op backend transition step)', () => {
    const group: MessageGroupWire = {
      ...baseGroup,
      meta: { thinking_steps: [{ text: '   ', timestamp_start: '2026-01-01 12:00:01' }] },
    };
    expect(normaliseAssistantMessage(group, [], undefined).toolActions).toEqual([]);
  });

  it('builds an llm-type toolAction from a non-empty thinking step', () => {
    const group: MessageGroupWire = {
      ...baseGroup,
      meta: {
        thinking_steps: [
          {
            text: 'reasoning...',
            timestamp_start: '2026-01-01 12:00:01',
            timestamp_finish: '2026-01-01 12:00:02',
            message: { id: 'm1', response_metadata: { model_name: 'gpt', tool_name: 'llm' } },
          },
        ],
      },
    };
    const result = normaliseAssistantMessage(group, [], undefined);
    expect(result.toolActions).toEqual([
      expect.objectContaining({ type: 'llm', name: 'llm', content: 'reasoning...', id: 'm1' }),
    ]);
  });

  it('builds a tool-type toolAction from a tool_calls entry, ordered before a later thinking step', () => {
    const group: MessageGroupWire = {
      ...baseGroup,
      meta: {
        thinking_steps: [{ text: 'after', timestamp_start: '2026-01-01 12:00:05' }],
        tool_calls: { c1: { tool_name: 'search', tool_run_id: 'r1', timestamp_start: '2026-01-01 12:00:01' } },
      },
    };
    const result = normaliseAssistantMessage(group, [], undefined);
    expect(result.toolActions?.map((action) => action.type)).toEqual(['tool', 'llm']);
    expect(result.toolActions?.[0]).toMatchObject({ name: 'search', id: 'r1' });
  });

  /*
   * The answer's own participant crosses the same wire in the same two
   * spellings, and this lookup is the ONLY thing that hands
   * `buildToolActions` the participant's `tools[]` — the fallback that
   * resolves a tool row's `toolkit_type`, and with it the icon the row renders.
   * A strict === here left every tool row of a numbered-id payload untyped.
   */
  it('resolves the answering participant across the two id spellings', () => {
    const toolCalls = { c1: { toolkit_name: 'jira', tool_name: 'search', tool_run_id: 'r1', timestamp_start: '2026-01-01 12:00:01' } };
    const tools = [{ name: 'jira', type: 'external' }];
    const numberedRow: MessageGroupWire = { ...baseGroup, author_participant_id: 3, meta: { tool_calls: toolCalls } };
    const spelledRow: MessageGroupWire = { ...baseGroup, author_participant_id: '3', meta: { tool_calls: toolCalls } };
    const spelledRoster: readonly MessageParticipantWire[] = [{ id: '3', meta: { tools } }];
    const numberedRoster = [{ id: 3, meta: { tools } }] as unknown as readonly MessageParticipantWire[];

    expect.soft(normaliseAssistantMessage(numberedRow, [], spelledRoster).toolActions?.[0]?.toolMeta).toMatchObject({
      toolkit_type: 'external',
    });
    expect.soft(normaliseAssistantMessage(spelledRow, [], numberedRoster).toolActions?.[0]?.toolMeta).toMatchObject({
      toolkit_type: 'external',
    });
  });

  it('builds hitlInterrupt from meta.hitl_interrupt, defaulting unset fields', () => {
    const group: MessageGroupWire = { ...baseGroup, meta: { hitl_interrupt: { message: 'Review this' } } };
    expect(normaliseAssistantMessage(group, [], undefined).hitlInterrupt).toMatchObject({
      message: 'Review this',
      available_actions: ['approve', 'reject'],
    });
  });

  it('falls hitlInterrupt back to the first hitl_interrupts entry when hitl_interrupt is absent', () => {
    const group: MessageGroupWire = {
      ...baseGroup,
      meta: { hitl_interrupts: [{ tool_call_id: 'tc1' }, { tool_call_id: 'tc2' }] },
    };
    const result = normaliseAssistantMessage(group, [], undefined);
    expect(result.hitlInterrupt).toMatchObject({ tool_call_id: 'tc1' });
    expect(result.hitlInterrupts).toHaveLength(2);
  });

  it('carries an ask_user pause’s questions and interrupt_id off the stored meta', () => {
    // The stored half of the same defect the stream half had: a pause reloaded
    // from the transcript rendered its question with no controls, because
    // `buildHitlInterruptFromRaw` wrote a closed field set that did not include
    // `questions`. `interrupt_id` travels with it — it is what
    // `findHitlInterruptId` needs to address a fan-out resume at all.
    const questions = [{ id: 'environment', question: 'Which environment?', options: [{ label: 'Staging' }] }];
    const group: MessageGroupWire = {
      ...baseGroup,
      meta: {
        hitl_interrupts: [
          {
            guardrail_type: 'clarifying_question',
            available_actions: ['answer'],
            tool_call_id: 'call_mock_ask_user_1',
            interrupt_id: 'int-ask-1',
            questions,
          },
        ],
      } as unknown as MessageGroupMetaWire,
    };
    const result = normaliseAssistantMessage(group, [], undefined);
    expect(result.hitlInterrupts?.[0]).toMatchObject({
      guardrail_type: 'clarifying_question',
      available_actions: ['answer'],
      interrupt_id: 'int-ask-1',
      questions,
    });
    // Anything that is not an array is dropped — the card maps over this.
    const bad: MessageGroupWire = {
      ...baseGroup,
      meta: { hitl_interrupts: [{ questions: 'nope' }] } as unknown as MessageGroupMetaWire,
    };
    expect(normaliseAssistantMessage(bad, [], undefined).hitlInterrupts?.[0]).toMatchObject({ questions: [] });
  });

  it('omits hitl_interrupts entirely when the raw array is empty (not an empty array)', () => {
    const group: MessageGroupWire = { ...baseGroup, meta: { hitl_interrupts: [] } };
    const result = normaliseAssistantMessage(group, [], undefined);
    expect('hitlInterrupt' in result).toBe(false);
    expect('hitlInterrupts' in result).toBe(false);
  });

  it('rebuilds every delegated-authorization card after a transcript reload', () => {
    const authorization = (interruptId: string, toolCallId: string) => ({
      interrupt_id: interruptId,
      tool_call_id: toolCallId,
      guardrail_type: 'mcp_auth',
      tool_name: 'SharePoint search',
      toolkit_type: 'sharepoint',
      server_url: 'https://sharepoint.example.test',
      resource_metadata: {
        resource_name: 'SharePoint',
        authorization_servers: ['https://login.example.test'],
      },
    });
    const group: MessageGroupWire = {
      ...baseGroup,
      meta: { authorization_requests: [authorization('auth-1', 'call-1'), authorization('auth-2', 'call-2')] },
    };

    const result = normaliseAssistantMessage(group, [], undefined);

    expect(result.toolActions?.map((action) => action.authorizationRequestId)).toEqual(['auth-1', 'auth-2']);
    expect(result.toolActions?.every((action) => action.status === 'action_required')).toBe(true);
  });
});
