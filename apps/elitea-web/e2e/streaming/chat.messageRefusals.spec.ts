/**
 * What the message-start route says when it will not run a turn.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's three message-request refusals — a wait timeout
 * outside its bounds, a conversation that does not exist, and a participant
 * that is not in the conversation. The legacy server answered all three with
 * one status and told them apart by a string in the body; this one draws the
 * line somewhere else, and where it draws it is the contract worth pinning:
 *
 *   400 `Invalid agent execution request` — the REQUEST is malformed. The
 *       execution contract is not one this route serves, or the body is not a
 *       start body (no question id, no user input, an attachment path that does
 *       not split). Nothing was looked up.
 *
 *   422 `unsupported_agent_execution` — the request is well formed and the
 *       admitted path cannot run THIS turn. The conversation, the caller's own
 *       participant row, the target participant or the version it names could
 *       not be resolved together. Which of them is missing is written to the
 *       deployment log, not to the caller: the reasons name joins, and a client
 *       cannot act on any of them differently.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY IT IS HERE AND NOT WITH THE OTHER API JOURNEYS
 * ─────────────────────────────────────────────────────────────────────────────
 * The route is registered only where the runtime plane is composed
 * (`internal/api/production_router.go` mounts it on `cfg.CurrentAgentStart`),
 * and the plain journeys stack composes none — so every one of these requests
 * arrives at the same "this path serves no POST" refusal there, and the three
 * cases cannot be told apart. `api.chat-contract.spec.ts` asserts the half that
 * stack CAN state: that the send is refused and stores nothing. This file
 * asserts the bodies.
 *
 * It runs no turn and needs no model. It is on `chat-stream` for the stack,
 * not for the runtime: nothing here reaches a worker.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE WAIT TIMEOUT
 * ─────────────────────────────────────────────────────────────────────────────
 * `await_task_timeout` was the legacy blocking-mode parameter: the server held
 * the request open for that many seconds and answered the settled exchange, and
 * a negative value was a validation error. This route does not block — it
 * admits the turn, answers a task handle and an events URL, and the tokens
 * arrive on the stream — so the parameter has no meaning here and is not part
 * of the contract. It is not silently honoured either, which is the failure a
 * port like this invites: a client that kept sending it would otherwise be
 * setting a deadline nobody reads. The case below states the property that
 * makes that safe — the parameter cannot change the answer.
 */
import { expect, test } from '@playwright/test';
import { randomUUID } from 'node:crypto';

import { BASE_URL } from '../../playwright.config';
import { readCallerPersonalProjectId } from '../fixtures/api';

const START_CONTRACT = 'agent.execute.application.v1';

/** One refusal, reduced to the two things a client can act on. */
interface Refusal {
  readonly status: number;
  readonly body: string;
  readonly error: string;
}

test('the message route names a malformed request and an unrunnable turn differently', async ({
  request,
}) => {
  const projectId = await readCallerPersonalProjectId(request);
  expect(projectId, 'this persona works in its own project, and the setup waits for one').not.toBe('');

  const api = `${BASE_URL}/api/v2/elitea_core`;
  const created = await request.post(`${api}/conversations/prompt_lib/${projectId}`, {
    data: { name: `autotest_refusals_${Date.now() % 1_000_000}` },
  });
  expect(created.status(), `the conversation was not created: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const conversation = (await created.json()) as { id?: string; uuid?: string };
  const conversationId = String(conversation.id ?? '');
  const conversationUuid = String(conversation.uuid ?? '');
  expect(conversationUuid, 'the message route addresses a conversation by uuid').not.toBe('');

  /** Send one start body and reduce the answer. */
  const send = async (
    uuid: string,
    body: Record<string, unknown>,
    contract = START_CONTRACT,
  ): Promise<Refusal> => {
    const response = await request.post(
      `${api}/messages/prompt_lib/${projectId}/${uuid}?execution_contract=${contract}`,
      { data: body },
    );
    const text = await response.text();
    let error = '';
    try {
      error = String((JSON.parse(text) as { error?: unknown }).error ?? '');
    } catch {
      error = '';
    }
    return { status: response.status(), body: text.slice(0, 300), error };
  };

  /** A start body that is well formed in every way the route checks first. */
  const wellFormed = (uuid: string, participantId: number): Record<string, unknown> => ({
    project_id: Number(projectId),
    conversation_uuid: uuid,
    participant_id: participantId,
    question_id: randomUUID(),
    payload: { user_input: 'autotest refusal probe' },
  });

  try {
    // ── malformed: the route serves no such contract ────────────────────
    const badContract = await send(
      conversationUuid,
      wellFormed(conversationUuid, 1),
      'agent.execute.nonsense.v9',
    );
    expect(badContract.status, `an unknown execution contract answered ${badContract.body}`).toBe(400);
    expect(badContract.error).toBe('Invalid agent execution request');

    // ── malformed: a start body with no question id ─────────────────────
    // The question id is the identity of the message group the turn writes,
    // so a body without one cannot be admitted at all — and it is refused
    // BEFORE anything is looked up, which is what makes it a 400 and not the
    // 422 below.
    const noQuestion = { ...wellFormed(conversationUuid, 1) };
    delete noQuestion['question_id'];
    const malformed = await send(conversationUuid, noQuestion);
    expect(malformed.status, `a start body with no question id answered ${malformed.body}`).toBe(400);
    expect(malformed.error).toBe('Invalid agent execution request');

    // ── unrunnable: the conversation does not exist ─────────────────────
    const nowhere = '00000000-0000-4000-8000-000000000000';
    const unknownConversation = await send(nowhere, wellFormed(nowhere, 1));
    expect(
      unknownConversation.status,
      `a turn in a conversation that does not exist answered ${unknownConversation.body}`,
    ).toBe(422);
    expect(unknownConversation.error).toBe('unsupported_agent_execution');

    // ── unrunnable: the participant is in no conversation ───────────────
    // The same refusal, deliberately: the two failures are one unresolved
    // join, and the reason that tells them apart goes to the deployment log.
    const strangerParticipant = await send(conversationUuid, wellFormed(conversationUuid, 99_999));
    expect(
      strangerParticipant.status,
      `a turn addressed to a participant of no conversation answered ${strangerParticipant.body}`,
    ).toBe(422);
    expect(strangerParticipant.error).toBe('unsupported_agent_execution');

    // ── the wait timeout cannot change any of this ──────────────────────
    // Same body, same conversation, same participant, plus a wait timeout the
    // legacy server rejected outright. The answer must be byte-for-byte the
    // one the request already earned: a parameter that changed it would be a
    // deadline this route does not implement, silently accepted.
    const withTimeout = await send(conversationUuid, {
      ...wellFormed(conversationUuid, 99_999),
      await_task_timeout: -20,
    });
    expect(withTimeout.status, 'an out-of-range wait timeout changed the status').toBe(
      strangerParticipant.status,
    );
    expect(withTimeout.error, 'an out-of-range wait timeout changed the refusal').toBe(
      strangerParticipant.error,
    );

    // ── and none of it wrote a message ──────────────────────────────────
    const transcript = await request.get(
      `${api}/messages/prompt_lib/${projectId}/${conversationId}?sort_order=asc&limit=100`,
    );
    expect(transcript.status()).toBe(200);
    expect(
      ((await transcript.json()) as { items?: readonly unknown[] }).items ?? [],
      'a refused start left the question in the transcript, with no answer ever coming',
    ).toEqual([]);
  } finally {
    await request.delete(`${api}/conversation/prompt_lib/${projectId}/${conversationId}`);
  }
});
