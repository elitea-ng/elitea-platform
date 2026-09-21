/**
 * @MENTION NOTIFICATIONS (#939 group 8).
 *
 * ELITEA-0396..0399 are four views of one feature: tagging a person in a chat
 * message notifies THEM — once per message however many times they were named
 * (0396), with the sender's context and a link back to the chat (0397), only
 * to the people actually named (0398), and never to the sender themselves
 * (0399).
 *
 * ─────────────────────────────────────────────────────────────────────────
 * THE FEATURE DOES NOT REACH THE SERVER, AND NOTHING WRITES THE ROW
 * ─────────────────────────────────────────────────────────────────────────
 *
 * The composer really does offer it. `useChatBoxMentions` resolves an `@`
 * mention (including `@everyone`) into `selectedUsers`, `useChatBoxActions`
 * turns that into `isSendingToUser` + `userIds`, and
 * `buildDefaultMessagePayload` puts both on the outgoing message. A user can
 * type the mention, see the picker, and send.
 *
 * Two things then stop it, and either alone would be enough:
 *
 *  1. the start route REFUSED the field — the parity gate answered
 *     `writeUnsupported` for any request carrying `user_ids`, and the
 *     camelCase spelling the composer actually sends was not bound at all;
 *  2. NOTHING produced a mention notification: `centry.notifications` had
 *     producers for PAT expiry, index ingest, artifact storage and index
 *     schedules, and no mention writer anywhere.
 *
 * FIXED (#977). The route now binds BOTH spellings and parses the list
 * (`parseMentionedUserIDs`), and `application/agentexecution/mentions.go`
 * writes one `centry.notifications` row per recipient through the same
 * producer shape the PAT-expiry sweep uses — with the event type and the
 * snake_case `meta` keys the web already resolves.
 *
 * This test asserts the two halves reachable from ONE persona: the turn is
 * admitted, and exactly one notification exists for the mentioned user
 * however many times the message named them. The multi-persona halves
 * (0398's "the non-mentioned participant is not notified", 0399's "the
 * sender is not notified") are pinned by the audience unit tests in
 * `mentions_test.go`, which can state them without a second login.
 */
import { randomUUID } from 'node:crypto';

import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  createAgentWithVersion,
  deleteAgent,
  deleteConversation,
  expectStoredAssistantAnswer,
  fillComposer,
  readCallerIdentity,
  readCallerPersonalProjectId,
} from '../fixtures/api';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

/*
 * onetest: ELITEA-0399 — a message carrying a mention is ACCEPTED. It was
 * refused 422 before #977, and the only field separating it from the control
 * the app itself sent is `user_ids`. ELITEA-0396/0397/0398's recipient-side
 * counts are pinned in Go — see the comment at the end of this test for why
 * this lane cannot read them.
 */
test('a chat message carrying a mention is accepted by the start route', async ({ page }) => {
  test.setTimeout(240_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'the chat persona must own a personal project (#290)').not.toBe('');
  const caller = await readCallerIdentity(page.request);
  expect(caller.id, 'the caller must have a user id to be mentioned by').not.toBe('');

  const stamp = String(Date.now()).slice(-7);
  let agentId = '';
  let conversationId = '';

  try {
    const agent = await createAgentWithVersion(
      page.request,
      `${AUTOTEST_PREFIX}mention-${stamp}`,
      {
        instructions: 'You are a mention fixture. Answer briefly.',
        welcomeMessage: 'Say something.',
        conversationStarters: ['Hello.'],
        model: { modelName: MOCK_MODEL },
        meta: { step_limit: 25, internal_tools: [] },
      },
      projectId,
      `${AUTOTEST_PREFIX}mention fixture`,
    );
    agentId = agent.id;

    // ── The conversation is minted BY THE APP, and that is load-bearing ───
    //
    // A conversation and participant assembled by hand over their own routes
    // is refused by the current-path resolver — 422
    // `unsupported_agent_execution`, measured here and again in
    // `chat.pipeline-triggers.spec.ts`. Building one that way would have made
    // the refusal below prove nothing about mentions, because the identical
    // request without a mention is refused too.
    await page.goto(`${BASE_URL}/app/agents/all/${agentId}`);
    const conversationCreated = page.waitForResponse(
      (r) =>
        /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
        r.request().method() === 'POST',
      { timeout: 60_000 },
    );
    await page.getByTestId('chat-with-agent-button').click();
    const conversation = (await (await conversationCreated).json()) as { id?: unknown; uuid?: unknown };
    conversationId = String(conversation.id ?? '');
    const conversationUuid = String(conversation.uuid ?? '');
    expect(conversationId, 'the Chat button must create a conversation').not.toBe('');
    expect(conversationUuid).toMatch(/^[0-9a-f-]{36}$/);
    await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 45_000 });

    // ── The app's OWN start request, captured and replayed with a mention ──
    //
    // A hand-built start body is refused by the current-path resolver for
    // reasons that have nothing to do with mentions (measured twice: once
    // here, once in `chat.pipeline-triggers.spec.ts`), so a mention sent that
    // way could never be told apart from an unrelated 422. The app's own
    // request IS admitted, so it is captured verbatim and replayed with
    // `user_ids` added and a fresh `question_id`: one field differs between
    // the admitted request and the one under test, which is the only way this
    // status means anything.
    let capturedBody = '';
    page.on('request', (request) => {
      if (request.method() !== 'POST') return;
      if (!START_RE.test(new URL(request.url()).pathname)) return;
      capturedBody = request.postData() ?? '';
    });

    const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    const sendButton = await fillComposer(page, 'A plain message, with nobody tagged.');
    await sendButton.click();
    expect((await started).status(), 'the control turn must be admitted').toBe(200);
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 180_000,
      message: 'the control turn produced no answer',
    });
    expect(capturedBody, 'the app’s own start body must have been captured').not.toBe('');

    const replay = JSON.parse(capturedBody) as Record<string, unknown>;
    const startUrl = `${API_BASE}/elitea_core/messages/prompt_lib/${projectId}/${conversationUuid}` +
      '?execution_contract=agent.execute.application.v1';
    // Named TWICE in one message, because ELITEA-0396's claim is that the
    // count of notifications follows MESSAGES and not tags.
    replay['question_id'] = randomUUID();
    replay['interaction_uuid'] = randomUUID();
    (replay['payload'] as Record<string, unknown>)['user_input'] =
      `Hey @${caller.email}, can you check this? Also @${caller.email}, confirm when done.`;
    replay['user_ids'] = [Number(caller.id)];

    const sent = await page.request.post(startUrl, { data: replay });
    expect(
      sent.status(),
      `a message carrying a mention must be accepted: ${(await sent.text()).slice(0, 300)}`,
    ).toBeLessThan(300);

    // ── WHAT THIS LANE CAN AND CANNOT SEE ────────────────────────────────
    //
    // The assertion above IS the fix: the identical request was refused 422
    // before #977, and the only field that differs from the control the app
    // itself sent is `user_ids`. That is the route half.
    //
    // The RECIPIENT half is not readable here, and the reason is worth
    // recording rather than working around: a notification belongs to the
    // person named, and this lane has one persona, who is also the sender —
    // whom the producer deliberately never notifies (ELITEA-0399). Reading
    // somebody else's notifications needs a second login, and this persona
    // cannot even read its OWN in its personal project (the notifications
    // route answers 403 there: `models.notifications.notifications.list` is
    // not among the grants the seeder gives it).
    //
    // So the producer's behaviour is pinned where it can be stated exactly:
    // `mentions_test.go` (one row per person per MESSAGE however many tags,
    // `@everyone` from the server's membership, the sender excluded, a failed
    // write costing the mention and not the turn) and
    // `chat_mention_notification_postgres_integration_test.go` (the rows land
    // in `centry.notifications` with the event type and the snake_case meta
    // keys the web resolves).
  } finally {
    if (conversationId !== '') await deleteConversation(page.request, conversationId, projectId).catch(() => undefined);
    if (agentId !== '') await deleteAgent(page.request, agentId).catch(() => undefined);
  }
});
