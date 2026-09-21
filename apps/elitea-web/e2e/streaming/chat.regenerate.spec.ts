/**
 * REGENERATE: a second answer to the SAME question, written OVER the first.
 *
 * The failure this exists to make impossible is the quiet one — a regeneration
 * that APPENDS. A transcript that grows a second assistant row per click looks
 * fine on screen for one click, and then the conversation the next turn is
 * conditioned on contains two contradictory answers to one question. Nothing in
 * the unit suite can see it: the shape lives in one Postgres statement
 * (`ResetCurrentAgentResponse`) and in what the browser actually posts.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE CONTRACT, AS MEASURED AGAINST THE RUNNING STACK
 * ─────────────────────────────────────────────────────────────────────────────
 * REPLACEMENT, in place, and the replacement is total:
 *
 *   POST /api/v2/elitea_core/regenerate/prompt_lib/{project}/{responseMessageUuid}
 *        ?execution_contract=agent.regenerate.v1
 *
 * answers 200 with `response_message_id` equal to the assistant row's OWN uuid.
 * Server-side, `ResetCurrentAgentResponse`
 * (`services/elitea-main/internal/db/queries/agent_chat.sql`) DELETEs that
 * group's `chat_message_items` and trace steps, strips its interrupt/skill
 * metadata, stamps a fresh `meta.execution_generation`, and flips
 * `is_streaming` back to TRUE — all on the row that already exists. No group is
 * inserted and none is removed, so the transcript stays at two rows and both
 * keep their `id`, `uid` and `created_at`.
 *
 * That last part is why row identity ALONE cannot carry this test: a
 * regeneration that never reached the model would also leave two rows with the
 * same ids. `meta.execution_generation` is the discriminator — it is set to the
 * `regeneration_id` the browser minted for THIS click, so the assertion below
 * ties the stored row to the exact button press that produced it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE QUESTION'S EDIT AFFORDANCE IS REACHABLE; THE EDITED-REGENERATE TURN IS NOT
 * ─────────────────────────────────────────────────────────────────────────────
 * `e2e/journeys/chat/chat.conversation.spec.ts` J9 drives "Edit the message and
 * regenerate answer" on the user's own bubble. This spec used to record that
 * that control was out of reach the moment a turn had been answered and
 * persisted. HALF of that is no longer true, and the header now records the fix:
 *
 *  1. THE UI OFFERS IT AGAIN — on the reader's OWN reloaded question.
 *     `ChatMessageList`'s `isEligibleForEdit` gates the control on the current
 *     author matching `message.userId`. It used to lose: a server-loaded
 *     transcript stated no author, so `normaliseUserMessage`'s
 *     `userOptionalFields` omitted `userId` (`entities/message/lib/normalise.ts`)
 *     and only the OPTIMISTIC send-path bubble ever carried one. That is fixed
 *     end to end — the transcript now serves `author_participant_id` per row,
 *     `normalise.ts` resolves the authoring participant to `message.userId` (as
 *     a `String()`), and `ChatMessageList` now `String()`-normalises BOTH sides
 *     of the check. That last step matters: the participants payload states the
 *     author id as a NUMBER (`entity_meta.id`) while `/social/author` states the
 *     current author id as a string, so a bare `===` between `6` and `"6"`
 *     missed and the reader's own reloaded question offered only "Copy to
 *     clipboard". Measured on the live stack after the fix: a hard reload's
 *     question bubble offers BOTH "Copy to clipboard" AND "Edit the message and
 *     regenerate answer". This spec asserts that VISIBILITY at the end — a
 *     UI-visibility check on the reloaded, server-authored question, nothing
 *     more.
 *  2. AN EDITED QUESTION IS NOW ACCEPTED (#980). The route used to refuse any
 *     body whose `updated_items` was non-empty (`!emptyJSONArray(body.
 *     UpdatedItems)` → 422 `unsupported_agent_execution`); it now parses
 *     exactly one `text_message` entry, REWRITES the stored question to that
 *     text (and stamps its `updated_at`) inside the same admission transaction
 *     that resets the answer, and runs the turn from the new text. The answer
 *     is still REWRITTEN IN PLACE, which is what this file is for — so the
 *     edited regeneration is asserted below on the same terms as the plain one:
 *     accepted, same answer uid, question now reading the edited text, and no
 *     extra row on either side. `buildRegenerateBody` no longer withholds the
 *     field, so the browser reaches the same contract this assertion posts.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE PRECONDITION THE PRODUCT DOES NOT STATE
 * ─────────────────────────────────────────────────────────────────────────────
 * A regeneration is refused 409 `agent_regeneration_pending` (`retryable: true`,
 * `Retry-After: 1`) while the answer's `chat_message_group.is_streaming` is
 * still TRUE, and that flag is cleared AFTER the answer text is written. So
 * both signals a user has — the answer is on screen, the composer is released —
 * can be true while the button they gate is still refused.
 *
 * THE UI USED TO SWALLOW THAT, and this file used to work around it. The
 * streamed regeneration reported a bare `false` for every failure, so
 * `createRegenerateAnswer` fell through to the pre-#93 REST trigger — which
 * posts no `execution_contract` and is answered 400 — then restored the
 * previous answer and warned to the console. The click did nothing and said
 * nothing, so this spec used to POLL `is_streaming` to the point where the
 * server would accept, and only then click (`waitForAnswerSettled`, deleted
 * with this change).
 *
 * The client now absorbs the window itself: `useChatStreamRunStarters`'
 * `regenerateDetailed` classifies that ONE refusal as `retry-later`, and
 * `createRegenerateAnswer` repeats the SAME request across a short bounded
 * ladder before giving up and writing the reason onto the answer card. So this
 * spec clicks the way a user does — the moment the button is offered — and
 * asserts the outcome instead of the precondition: any 409 seen on the way is
 * required to be the retryable one, and the click is required to end in a 200.
 * If the retry is ever removed, this races exactly as a user's click would and
 * goes red, which is the whole point of dropping the poll.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT NO BACKEND HERE CAN DISCRIMINATE
 * ─────────────────────────────────────────────────────────────────────────────
 * The regenerated TEXT is deliberately not required to differ from the first
 * answer, and not required to match it either. The offline mock is
 * deterministic — a second run of the same question returns the same bytes, so
 * "the text changed" would fail against a perfectly correct regeneration. A
 * real model is the opposite: it may or may not repeat itself, so "the text is
 * unchanged" is equally unassertable. Everything this file claims about the
 * answer therefore comes from what the SERVER recorded about the run, not from
 * comparing two model outputs.
 *
 * WHY IT LIVES HERE: `journeys/**` runs against
 * `docker-compose.e2e-standalone.yml`, which has no runtime plane, no worker
 * and no model — no assistant message ever arrives there, so no answer-side
 * Regenerate control can render at all. The `chat-stream` project matches
 * `streaming/chat.*` and runs against the full standalone stack via
 * `scripts/chat-stream-e2e.sh`, serially (`--workers=1`).
 */
import { randomUUID } from 'node:crypto';

import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { expectStoredAssistantAnswer, readStoredTranscript } from '../fixtures/api';

/** Matched WITHOUT a project id: the chat driver acts inside its own personal project (#290). */
const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const REGENERATE_RE = /\/elitea_core\/regenerate\/prompt_lib\/(\d+)\/([0-9a-f-]+)$/;

/** The model `seed-llm` seeds; `E2E_CHAT_MODEL` names a real one an operator has instead. */
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

/** `ChatBox` names the conversation after the question, truncated to 50 chars. */
const MAX_NAME = 50;

/** The run's own token — see `chat.multiturn.spec.ts` for why `Date.now()` alone is not enough. */
function uniqueToken(tag: string): string {
  return `${tag}${Date.now().toString(36)}${Math.floor(Math.random() * 46_656).toString(36)}`;
}

test('regenerating rewrites the SAME answer row rather than appending a second one', async ({ page }) => {
  // Two model calls — the turn and its regeneration — plus stack round trips.
  // Every wait below is bounded well under this, so a real hang fails on its
  // own step rather than on the clock.
  test.setTimeout(360_000);

  const token = uniqueToken('rg');
  const prompt = `autotest echo exactly: ${token}`;
  expect(prompt.length, 'the prompt must fit the 50-char conversation-name truncation').toBeLessThanOrEqual(MAX_NAME);

  // Counted for the whole test. A "regeneration" that quietly opened a new turn
  // — a fresh conversation, or a second start POST — would leave the transcript
  // assertions below satisfiable in shapes this catches and they do not.
  const conversationCreates: string[] = [];
  const starts: string[] = [];
  page.on('response', (response) => {
    const path = new URL(response.url()).pathname;
    const method = response.request().method();
    if (method !== 'POST') return;
    if (CONVERSATIONS_RE.test(path)) conversationCreates.push(`${response.status()} ${path}`);
    if (START_RE.test(path)) starts.push(`${response.status()} ${path}`);
  });

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('menuitem').filter({ hasText: MODEL_NAME }).first();
  await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
  await modelOption.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

  // ── One completed turn ──────────────────────────────────────────────────
  const created = page.waitForResponse(
    (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );

  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  await input.fill(prompt);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();

  const createdResponse = await created;
  expect(createdResponse.status(), 'the send must create a real conversation').toBe(201);
  const projectId = CONVERSATIONS_RE.exec(new URL(createdResponse.url()).pathname)?.[1] ?? '';
  const conversation = (await createdResponse.json()) as { id?: string; uuid?: string };
  const conversationId = conversation.id ?? '';
  expect(conversationId, 'the conversation must carry a server-assigned id').toMatch(/^\d+$/);

  const startResponse = await started;
  expect(
    startResponse.status(),
    `the turn to be regenerated was itself refused: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);

  // The mock ECHOES the prompt, so `contains: token` proves the stored answer
  // belongs to THIS turn. A real model echoes nothing, and against one the
  // assertion falls back to "the turn stored a non-error answer" — the same
  // stated relaxation chat.streaming.spec.ts makes, keyed on the same
  // variable, rather than a silent weakening for everyone. Everything the
  // regeneration half asserts below is structural already (a new
  // `execution_generation` on the SAME row), which is why this spec runs in
  // the `chat-stream-real` project and the echo-bound ones do not.
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 120_000,
    message: 'there is no answer to regenerate — the first turn never stored one',
    ...(process.env['E2E_CHAT_MODEL'] ? {} : { contains: token }),
  });

  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 60_000 });
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 120_000 });

  // The transcript as it stands, captured BEFORE the click. Every claim about
  // what regeneration did is a comparison against this, not against a
  // hardcoded expectation of what a conversation looks like.
  const before = await readStoredTranscript(page, projectId, conversationId);
  expect(
    before.map((row) => row.role),
    'the conversation must hold exactly one question and one answer before regenerating',
  ).toEqual(['user', 'assistant']);
  const questionBefore = before[0];
  const answerBefore = before[1];
  const generationBefore = String(answerBefore?.metadata['execution_generation'] ?? '');
  expect(
    generationBefore,
    'the completed answer must record which run produced it, or nothing can tell a regeneration from a no-op',
  ).not.toBe('');

  // ── The control ─────────────────────────────────────────────────────────
  // Hovered, not clicked blind: the action row is `visibility: hidden` until
  // its answer block is hovered (`ApplicationAnswer.tsx`), and `visibility`
  // removes an element from the accessibility tree — so a Regenerate button
  // that exists but never becomes reachable is a defect this step surfaces as
  // "not visible" rather than passing on a DOM node no user could press.
  const answerCard = page.getByTestId('application-answer').first();
  await answerCard.getByTestId('skill-test-last-response').hover();
  const regenerate = answerCard.getByRole('button', { name: 'Regenerate' });
  await expect(
    regenerate,
    'a completed answer must offer Regenerate — it is gated on the turn no longer streaming',
  ).toBeVisible({ timeout: 20_000 });
  await expect(regenerate, 'Regenerate must be usable once the turn has finished').toBeEnabled();

  // Every POST this click produces, not just the first. The client retries the
  // still-finalizing 409 itself now, so the run may be admitted on the second or
  // third request — and the ones before it are not failures, they are the
  // mechanism. `refusals` collects them so the shape of each is asserted rather
  // than assumed: a 409 that is NOT the retryable one would mean the client is
  // repeating a request that can never succeed.
  const refusals: { status: number; error: string; retryable: unknown }[] = [];
  page.on('response', (response) => {
    if (!REGENERATE_RE.test(new URL(response.url()).pathname)) return;
    if (response.request().method() !== 'POST' || response.status() !== 409) return;
    void response
      .json()
      .then((body: { error?: string; retryable?: unknown }) => {
        refusals.push({ status: 409, error: body.error ?? '', retryable: body.retryable });
      })
      .catch(() => undefined);
  });

  const regenerated = page.waitForResponse(
    (r) =>
      REGENERATE_RE.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST' &&
      r.status() !== 409,
    { timeout: 60_000 },
  );
  await regenerate.click();

  const regenerateResponse = await regenerated;
  expect(
    regenerateResponse.status(),
    `the regeneration was refused: ${(await regenerateResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  // Whatever the timing produced, every refusal on the way must have been the
  // one the client is entitled to repeat. `Retry-After: 1` and `retryable: true`
  // are the server's own statement that repeating works; a `retryable: false`
  // 409 here would be the client looping on something it cannot fix.
  for (const refusal of refusals) {
    expect(
      refusal,
      'the client may only repeat a regeneration the server marked retryable',
    ).toMatchObject({ error: 'agent_regeneration_pending', retryable: true });
  }

  // The SSE contract, opted into by query parameter. Without it the Go route
  // answers 400 outright, so a client that dropped the parameter would fall
  // silently back to a socket this stack does not run.
  const regenerateUrl = new URL(regenerateResponse.url());
  expect(
    regenerateUrl.searchParams.get('execution_contract'),
    'regeneration must opt into the current execution contract',
  ).toBe('agent.regenerate.v1');
  // The route's last path segment is the answer being replaced. This is the
  // single strongest statement of "replacement": the request names an EXISTING
  // message rather than asking for a new one.
  expect(
    REGENERATE_RE.exec(regenerateUrl.pathname)?.[2],
    'the regeneration must address the answer that already exists',
  ).toBe(answerBefore?.uid);

  const regenerateBody = (await regenerateResponse.json()) as { events_url?: string; response_message_id?: string };
  expect(regenerateBody.events_url, 'the regeneration must hand back a stream to read').toMatch(
    /^\/api\/v2\/executions\/\d+\/[0-9a-f]+\/events$/,
  );
  expect(
    regenerateBody.response_message_id,
    'the server must confirm it is rewriting the same answer, not creating one',
  ).toBe(answerBefore?.uid);

  // The id the BROWSER minted for this click. It becomes the row's
  // `meta.execution_generation` server-side (`CurrentRegenerateTurn.ExecutionGeneration`
  // ← `request.RegenerationID`), which is what lets the stored row be tied back
  // to this exact button press below.
  const sentBody = JSON.parse(regenerateResponse.request().postData() ?? '{}') as {
    regeneration_id?: string;
    updated_items?: unknown[];
    message_id?: string;
  };
  expect(sentBody.regeneration_id, 'the client must mint an id for this regeneration').toMatch(
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/,
  );
  expect(sentBody.message_id, 'the body must name the answer the URL names').toBe(answerBefore?.uid);

  // ── The replacement, in the store ───────────────────────────────────────
  // Polled on the generation stamp rather than on the text: mid-run the row is
  // legitimately empty (the reset DELETEs its items first), so "content is
  // non-empty" alone would race the write-back, and "content changed" is not
  // assertable at all against a deterministic mock.
  await expect
    .poll(
      async () => {
        const rows = await readStoredTranscript(page, projectId, conversationId);
        const answer = rows.find((row) => row.uid === answerBefore?.uid);
        if (answer === undefined || answer.content.trim() === '') return '';
        return String(answer.metadata['execution_generation'] ?? '');
      },
      {
        timeout: 180_000,
        message:
          'the regenerated answer never finished: the row is still empty, or still carries the ' +
          'generation stamp of the run it was supposed to replace',
      },
    )
    .toBe(sentBody.regeneration_id);

  const after = await readStoredTranscript(page, projectId, conversationId);

  // REPLACEMENT, not append — the whole point of the file.
  expect(
    after.map((row) => `${row.role}:${row.uid}`),
    'regenerating must rewrite the existing answer: the transcript must still be one question and one answer, ' +
    'with the same identities',
  ).toEqual([`user:${questionBefore?.uid ?? ''}`, `assistant:${answerBefore?.uid ?? ''}`]);

  // The question is untouched. A PLAIN regeneration (no `updated_items`) re-runs
  // the question as stored, so one that rewrote it would be rewriting history.
  // The EDITED case — where rewriting it is the point — is asserted further
  // down, against the same answer row.
  expect(after[0]?.content, 'regenerating must not alter the question').toBe(questionBefore?.content);

  const answerAfter = after[1];
  expect(answerAfter?.isError, 'the regenerated answer must not be a stored refusal').toBe(false);
  expect(
    (answerAfter?.content ?? '').trim().length,
    'the regenerated answer must not be left empty — the reset clears the row before the run refills it',
  ).toBeGreaterThan(0);
  expect(
    String(answerAfter?.metadata['execution_generation'] ?? ''),
    'the answer must record the NEW run, not the one it replaced',
  ).not.toBe(generationBefore);

  // Nothing else moved. `message_count` is a `COUNT(*)` over
  // `chat_message_group` computed by the conversation's own detail query — an
  // independent witness that the regeneration inserted no group and deleted
  // none.
  const detail = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`,
  );
  expect(detail.status(), 'the conversation must still be readable at its own route').toBe(200);
  expect(
    ((await detail.json()) as { message_count?: number }).message_count,
    'a regeneration must not change how many message groups the conversation holds',
  ).toBe(2);

  expect(conversationCreates, 'regenerating must not open another conversation').toHaveLength(1);
  expect(
    starts,
    'regenerating must go through the regeneration route, never through a fresh turn start',
  ).toHaveLength(1);

  // ── An EDITED question: accepted, and applied to the SAME rows (#980) ───
  // Posted with the SAME shape the browser just used, changed in exactly one
  // field: a non-empty `updated_items`. This used to be refused with 422
  // `unsupported_agent_execution`; the contract now is that the route parses
  // the single `text_message` entry, rewrites the stored question to that text
  // (and stamps its `updated_at`) inside the admission transaction that resets
  // the answer, and re-runs the turn from the NEW text. Asserted here, on the
  // same conversation, so that the file's whole claim — replacement, never
  // append — is proven for the edited path too, not only the plain one.
  const editedPrompt = `${prompt} edited`;
  const editedRegenerationId = randomUUID();
  // 409 `agent_regeneration_pending` is a RETRYABLE refusal by contract (the
  // route sets `Retry-After: 1` and `"retryable": true`): the run that just
  // finished is still being finalized for a moment after its row has settled,
  // and a client is expected to come back rather than treat it as a failure.
  // Retried here for the same reason, so this assertion measures the EDITED
  // contract and not that lag; anything else fails on the first answer.
  let editedStatus = 0;
  let editedResponseBody = '';
  for (let attempt = 0; attempt < 30; attempt += 1) {
    const response = await page.request.post(
      `${BASE_URL}/api/v2/elitea_core/regenerate/prompt_lib/${projectId}/${answerBefore?.uid ?? ''}` +
      '?execution_contract=agent.regenerate.v1',
      {
        headers: { 'Content-Type': 'application/json' },
        data: {
          ...sentBody,
          regeneration_id: editedRegenerationId,
          // No `uuid`: the route then rewrites the question's OWN current text
          // item, which is the branch a message with no stored item id takes.
          // The browser sends the item's uuid (`questionItem.uuid` in
          // `UserMessage.tsx`, NOT the message uid — a message uid here names
          // no item and is refused 400, which is the contract working), and
          // that path is driven end to end by `chat.edit-user-message.spec.ts`
          // (ELITEA-0545). This file's subject is the ROW IDENTITY either way.
          updated_items: [{ content: editedPrompt, item_type: 'text_message' }],
        },
      },
    );
    editedStatus = response.status();
    editedResponseBody = await response.text();
    if (editedStatus !== 409 || !editedResponseBody.includes('agent_regeneration_pending')) break;
    await page.waitForTimeout(1_000);
  }
  expect(
    editedStatus,
    `a regeneration carrying an edited question must be accepted and run from the new text: ${editedResponseBody}`,
  ).toBe(200);
  const editedBody = JSON.parse(editedResponseBody) as { response_message_id?: string };
  expect(
    editedBody.response_message_id,
    'an edited regeneration must rewrite the answer it names, not create a second one',
  ).toBe(answerBefore?.uid);

  // The rewrite is part of the admission transaction, so the stored question
  // carries the new text as soon as the request has been answered — polled
  // only because the read is a separate round trip.
  await expect
    .poll(
      async () => (await readStoredTranscript(page, projectId, conversationId))[0]?.content,
      { timeout: 30_000, message: 'the stored question was never rewritten to the edited text' },
    )
    .toBe(editedPrompt);

  // …on the SAME rows: one question, one answer, both keeping their identities.
  const afterEdit = await readStoredTranscript(page, projectId, conversationId);
  expect(
    afterEdit.map((row) => `${row.role}:${row.uid}`),
    'an edited regeneration must rewrite the question and the answer in place, adding no row',
  ).toEqual([`user:${questionBefore?.uid ?? ''}`, `assistant:${answerBefore?.uid ?? ''}`]);

  // And it really ran: the answer row settles carrying THIS regeneration's
  // stamp. Without this the test would leave a turn in flight under the reload
  // below, and "accepted" would prove only that the route said 200.
  await expect
    .poll(
      async () => {
        const rows = await readStoredTranscript(page, projectId, conversationId);
        const answer = rows.find((row) => row.uid === answerBefore?.uid);
        if (answer === undefined || answer.content.trim() === '') return '';
        return String(answer.metadata['execution_generation'] ?? '');
      },
      {
        timeout: 180_000,
        message: 'the edited regeneration never finished: the answer row is still empty or still carries the old stamp',
      },
    )
    .toBe(editedRegenerationId);

  // ── The question's edit affordance is now REACHABLE on reload ────────────
  // Header point 1: this used to read "the UI does not offer it" — a
  // server-loaded transcript stated no author, `isEligibleForEdit` was false,
  // and the question bubble offered only "Copy to clipboard". Now the transcript
  // serves `author_participant_id`, `normalise.ts` resolves it to
  // `message.userId`, and `ChatMessageList` String()-normalises the author
  // check, so the reader's OWN question offers the edit control again.
  //
  // A HARD RELOAD is deliberate: it re-reads the transcript from the server so
  // this asserts against the PERSISTED, server-authored question, not the
  // optimistic bubble the send path stamped the user onto (the only reason J9
  // could ever reach this control). This is a UI-VISIBILITY assertion only —
  // the edited-regenerate ROUND TRIP is driven over the route just above, and
  // the browser's own edit-and-save path is `chat.edit-user-message.spec.ts`'s
  // subject (ELITEA-0545), so it is not duplicated here.
  //
  // The bubble is matched on the EDITED text: the accepted regeneration above
  // rewrote the stored question, so after this reload that is what the server
  // serves — matching on the original text would be asserting the rewrite did
  // not happen.
  await page.reload();
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 120_000 });
  const ownQuestion = page
    .getByTestId('user-message')
    .filter({ hasText: editedPrompt })
    .first();
  await expect(
    ownQuestion,
    "the reloaded transcript must still render the reader's own question",
  ).toBeVisible({ timeout: 60_000 });

  // The action row is `visibility: hidden` until its bubble is hovered
  // (`UserMessage.tsx`'s `&:hover .actionButtons`), and `visibility` drops the
  // node from the accessibility tree — so an edit control that exists but never
  // becomes reachable surfaces here as "not visible" rather than passing on a
  // node no user could press. When the reader is NOT the author, the control is
  // not rendered at all, so this same assertion would fail closed.
  await ownQuestion.hover();
  const editControl = ownQuestion.getByRole('button', { name: 'Edit the message and regenerate answer' });
  await expect(
    editControl,
    "a reader's OWN persisted question must offer the edit affordance once the transcript is reloaded from the server",
  ).toBeVisible({ timeout: 20_000 });
  await expect(editControl, 'the edit affordance must be usable, not merely present').toBeEnabled();
});
