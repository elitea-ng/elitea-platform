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
 *  1. THE START ROUTE REFUSES THE FIELD. `currentApplicationStartBody`
 *     declares `UserIDs json.RawMessage \`json:"user_ids"\`` and the parity
 *     gate answers `writeUnsupported` when it is present at all —
 *     `!absentJSON(body.UserIDs)` in
 *     `services/elitea-main/internal/api/v2/agentexecution/route.go`. So a
 *     turn that carries a mention is refused outright; a turn that does not
 *     carries no mention.
 *  2. NOTHING PRODUCES A MENTION NOTIFICATION. `centry.notifications` is
 *     written by the PAT-expiry sweep, the index-ingest path, the artifact
 *     storage path and the index schedules — and by nothing else. A search of
 *     `services/elitea-main` for a mention producer finds no writer at all,
 *     so even a mention that reached the server would notify no one.
 *
 * The test below is written as the cases say it should behave and is marked
 * against #977. It asserts the two halves that are reachable from one
 * persona — the turn is admitted, and a notification row exists for the
 * mentioned user — because the multi-persona halves (0398's "the
 * non-mentioned participant is not notified", 0399's "the sender is not
 * notified") cannot even begin while the first send is refused.
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
 * onetest: ELITEA-0399 (a tagged user receives an in-app notification),
 * ELITEA-0396 (one notification however many times the same user is named),
 * ELITEA-0397 (the notification carries the sender's context and points at the
 * chat) and ELITEA-0398 (only the named users are notified). One journey for
 * four cases: all four are views of a message that this platform refuses to
 * accept, so each would fail at the same first step.
 */
test('mentioning a user in a chat message notifies them once', async ({ page }) => {
  test.fail(
    true,
    '#977: product gap — the composer resolves @mentions into `userIds`, but the start route refuses any request carrying `user_ids` (writeUnsupported) and nothing in elitea-main writes a mention notification, so a tagged user is never told',
  );
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

    // ── CONTROL: this conversation accepts an ordinary turn ───────────────
    // Sent through the app, so it is the shape the resolver admits. It is what
    // makes the refusal below a statement about the MENTION FIELD and not
    // about the fixture.
    const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    const sendButton = await fillComposer(page, 'A plain message, with nobody tagged.');
    await sendButton.click();
    expect(
      (await started).status(),
      'the control turn must be admitted, or the refusal below says nothing about mentions',
    ).toBe(200);
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 180_000,
      message: 'the control turn produced no answer',
    });

    // The participant the app targeted, read back from the conversation the
    // app created.
    const details = await page.request.get(
      `${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`,
    );
    expect(details.ok(), 'the conversation details must be readable').toBe(true);
    const participants = ((await details.json()) as { participants?: readonly { id?: unknown }[] }).participants ?? [];
    const participantId = Number(participants[0]?.id ?? 0);
    expect(participantId, 'the app-created conversation must carry a participant').toBeGreaterThan(0);

    // ── The message that names somebody, TWICE ────────────────────────────
    // Twice, because ELITEA-0396's whole claim is that the count of
    // notifications follows the number of MESSAGES and not the number of
    // tags. `user_ids` is the field the composer's mention state becomes
    // (`buildDefaultMessagePayload`), carried here in the shape the start
    // route declares for it.
    const sent = await page.request.post(
      `${API_BASE}/elitea_core/messages/prompt_lib/${projectId}/${conversationUuid}` +
        '?execution_contract=agent.execute.application.v1',
      {
        data: {
          payload: { user_input: `Hey @${caller.email}, can you check this? Also @${caller.email}, confirm when done.`, attachments: [] },
          project_id: Number(projectId),
          participant_id: participantId,
          conversation_uuid: conversationUuid,
          question_id: randomUUID(),
          interaction_uuid: randomUUID(),
          attachments_info: [],
          mcp_tokens: {},
          user_ids: [Number(caller.id)],
        },
      },
    );
    expect(
      sent.status(),
      `a message carrying a mention must be accepted: ${(await sent.text()).slice(0, 300)}`,
    ).toBeLessThan(300);

    // ── Exactly one notification for the mentioned user ───────────────────
    await expect
      .poll(
        async () => {
          const response = await page.request.get(
            `${API_BASE}/elitea_core/notifications/notifications/prompt_lib/${projectId}`,
          );
          if (!response.ok()) return -1;
          const body = (await response.json()) as { rows?: readonly unknown[]; items?: readonly unknown[] };
          const rows = body.rows ?? body.items ?? [];
          return rows.filter((row) => JSON.stringify(row).includes(conversationUuid)).length;
        },
        {
          timeout: 60_000,
          message: 'the mentioned user was never notified',
        },
      )
      .toBe(1);
  } finally {
    if (conversationId !== '') await deleteConversation(page.request, conversationId, projectId).catch(() => undefined);
    if (agentId !== '') await deleteAgent(page.request, agentId).catch(() => undefined);
  }
});
