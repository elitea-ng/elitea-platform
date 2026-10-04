/**
 * The SECOND turn: a conversation that is still usable once its first answer
 * has landed.
 *
 * `chat.streaming.spec.ts` proves ONE turn works and then navigates away, so
 * everything that only breaks on the way from turn 1 to turn 2 is invisible to
 * it. That gap is not hypothetical on this stack:
 *
 *  - the send path NAVIGATES. `pages/chat/index.tsx`'s `handleConversationCreated`
 *    routes to `/app/chat/{id}` the moment the first send's conversation comes
 *    back, so the composer the second question is typed into belongs to a
 *    REMOUNTED ChatBox reading the conversation off the server. A model
 *    selection, a participant or a conversation identity lost across that
 *    remount produces a second send that is refused, or — worse — one that
 *    opens a NEW conversation and leaves the user looking at a transcript that
 *    silently forked.
 *  - the transcript is what the next turn is CONDITIONED on. A history that
 *    does not accumulate is not a cosmetic defect: the model is answering a
 *    different conversation than the one on screen.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT MAKES THESE ASSERTIONS DISCRIMINATING
 * ─────────────────────────────────────────────────────────────────────────────
 * Every claim below is read back from the SERVER, never from the screen:
 *
 *  - the second send is required to be ADMITTED (a start POST answering 200),
 *    not merely to have cleared the composer;
 *  - `POST …/conversations/prompt_lib/{project}` is counted for the whole test
 *    and must have happened EXACTLY ONCE. That is the assertion that says
 *    "same conversation" — a second turn that forked into a fresh conversation
 *    would otherwise satisfy every other check in this file, because the fork
 *    also has one user row and one assistant row;
 *  - the stored transcript is read in `asc` order and asserted ROW BY ROW, so
 *    a run that stored the reply to the first question twice, or answered B
 *    before A, fails on the row it got wrong;
 *  - `message_count` on the conversation's own detail response counts
 *    `chat_message_group` (see the Go repo's `Get`), so it is 4 after two
 *    turns — 2 questions + 2 replies — and is a second, independently computed
 *    witness that nothing was dropped or double-written.
 *
 * WHY IT LIVES HERE: `journeys/**` runs against
 * `docker-compose.e2e-standalone.yml`, which has no runtime plane, no worker
 * and no model backend — an agent turn cannot happen there at all, let alone
 * two. The `chat-stream` project matches `streaming/chat.*` and runs against
 * the full standalone stack via `scripts/chat-stream-e2e.sh`, serially
 * (`--workers=1`), because elitea-main admits only four concurrent replay
 * streams per principal and every journey here signs in as the same persona.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { expectStoredAssistantAnswer, readStoredTranscript } from '../fixtures/api';

/**
 * Route shapes, matched WITHOUT pinning a project id — the chat driver acts
 * inside its own personal project (#290), so the id is read back off the app's
 * own requests rather than hardcoded to 1.
 */
const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/**
 * The model `seed-llm` seeds into every personal project, overridable the same
 * way `chat.streaming.spec.ts` overrides it: `E2E_CHAT_MODEL` names the model
 * an operator actually has seeded (a real vLLM/Qwen deployment), and CI leaves
 * it unset so the offline mock is what runs there.
 */
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

/** `ChatBox` names the conversation after the FIRST question, truncated to 50 chars. */
const MAX_NAME = 50;

/**
 * A token this run — and only this run — can produce.
 *
 * Both questions ASK for the token back verbatim. That is what makes the
 * echo assertion hold against both backends this project is driven with: the
 * offline mock echoes its input unconditionally, and a real instruction-following
 * model returns a short opaque token when told to. Without the token, "the
 * turn produced text" cannot tell the second answer from the first one
 * re-rendered, which is precisely the defect this file is about.
 *
 * `Date.now()` alone is not enough: other streaming journeys may be driven
 * against the same stack in the same second, and a shared token would let one
 * run's transcript satisfy another's assertion.
 */
function uniqueToken(tag: string): string {
  return `${tag}${Date.now().toString(36)}${Math.floor(Math.random() * 46_656).toString(36)}`;
}

test('a second turn lands in the SAME conversation and the transcript accumulates', async ({ page }) => {
  // TWO whole agent turns — conversation create, admission, dispatch to the
  // worker, a model call and the stream back, twice. Every wait below is
  // bounded well under this, so a real hang fails on its own step rather than
  // on the clock.
  test.setTimeout(360_000);

  const tokenA = uniqueToken('mta');
  const tokenB = uniqueToken('mtb');
  const promptA = `autotest echo exactly: ${tokenA}`;
  const promptB = `autotest echo exactly: ${tokenB}`;
  expect(promptA.length, 'the first prompt must fit the 50-char conversation-name truncation').toBeLessThanOrEqual(
    MAX_NAME,
  );

  // ── The fork guard ──────────────────────────────────────────────────────
  // Counted for the WHOLE test, and armed before the first navigation. A
  // second turn that opened a fresh conversation instead of continuing this
  // one would pass every per-turn assertion below — it has its own question
  // and its own answer — and would be caught only here.
  const conversationCreates: string[] = [];
  page.on('response', (response) => {
    if (CONVERSATIONS_RE.test(new URL(response.url()).pathname) && response.request().method() === 'POST') {
      conversationCreates.push(`${response.status()} ${response.url()}`);
    }
  });

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  // The seeded model is not decoration: an ad-hoc turn resolves against a
  // `dummy` participant carrying the model, and the start route reads
  // `llm_settings.model_name`. With nothing selected the send is rejected 400
  // before it reaches the worker.
  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('option').filter({ hasText: MODEL_NAME }).first();
  await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
  await modelOption.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

  // ── Turn 1 ──────────────────────────────────────────────────────────────
  const created = page.waitForResponse(
    (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  const startedA = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );

  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  await input.fill(promptA);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();

  const createdResponse = await created;
  expect(createdResponse.status(), 'the first send must create a real conversation').toBe(201);
  const projectId = CONVERSATIONS_RE.exec(new URL(createdResponse.url()).pathname)?.[1] ?? '';
  expect(projectId, 'the conversation must belong to a project').not.toBe('');
  const conversation = (await createdResponse.json()) as { id?: string; uuid?: string; name?: string };
  const conversationId = conversation.id ?? '';
  expect(conversationId, 'the conversation must carry a server-assigned id').toMatch(/^\d+$/);
  expect(conversation.name, 'the conversation is named after the question that opened it').toBe(promptA);

  const startAResponse = await startedA;
  expect(
    startAResponse.status(),
    `the first turn was refused: ${(await startAResponse.text()).slice(0, 300)}`,
  ).toBe(200);

  // The store, not the screen — and `contains` inside the poll, so this cannot
  // settle for "some assistant row exists". The `is_error` discrimination (a
  // refused turn is STORED as an assistant row) lives in the helper.
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 120_000,
    message: 'the first answer was never stored, or did not answer THIS question',
    contains: tokenA,
  });

  // ── The remount the second turn has to survive ──────────────────────────
  // `handleConversationCreated` navigates here. Waited for EXPLICITLY: typing
  // into the composer mid-navigation types into a component that is about to
  // be replaced, and the resulting "the send did nothing" failure would be a
  // statement about this test rather than about the feature.
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 60_000 });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  // The model must have survived the remount. It is asserted rather than
  // re-selected: re-picking it here would repair the very state whose loss
  // makes the second send fail, and the failure would then surface as an
  // unrelated 400 from the start route.
  await expect(
    page.getByTestId('model-selector-name'),
    'the model selection must survive the navigation the first send performs',
  ).toContainText(MODEL_NAME, { timeout: 30_000 });

  // ── Turn 2 ──────────────────────────────────────────────────────────────
  // The composer releasing is the state the product uses to say "your turn
  // again", so it is what gates the second send — not a sleep. `toBeEditable`
  // fails on the disabled-while-streaming composer, so a run that never
  // settles fails here naming that, rather than timing out later on a missing
  // second answer.
  const composer = page.getByTestId('chat-message-input');
  await expect(composer, 'the composer must be released before a second question is possible').toBeEditable({
    timeout: 120_000,
  });
  // The send control does not exist while the composer is empty; a stub that
  // always paints one fails here.
  await expect(page.getByTestId('chat-send-button')).toHaveCount(0);

  const startedB = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );

  await composer.fill(promptB);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();

  const startBResponse = await startedB;
  expect(
    startBResponse.status(),
    `the second turn was refused: ${(await startBResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  // The second start must address the SAME conversation. The route's last
  // segment is the conversation uuid, so this catches a fork that reused the
  // route while the conversation-create counter was still at one — a shape a
  // count alone cannot see.
  expect(
    new URL(startBResponse.url()).pathname.split('/').pop(),
    'the second turn must be started against the conversation the first one created',
  ).toBe(conversation.uuid ?? '');

  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 120_000,
    message: 'the second answer was never stored, or did not answer the SECOND question',
    contains: tokenB,
  });

  // ── The accumulated transcript ──────────────────────────────────────────
  // Read in `asc` order (the route defaults to DESC) and asserted row by row.
  // Polled only for the COUNT: the second answer is already known to be
  // stored, but the pair is written by two statements and a read that lands
  // between them sees three rows — which would read as "a message was lost".
  await expect
    .poll(async () => (await readStoredTranscript(page, projectId, conversationId)).length, {
      timeout: 30_000,
      message: 'the conversation must hold both turns: 2 questions and 2 answers',
    })
    .toBe(4);

  const transcript = await readStoredTranscript(page, projectId, conversationId);
  expect(
    transcript.map((row) => row.role),
    'the transcript must read question, answer, question, answer',
  ).toEqual(['user', 'assistant', 'user', 'assistant']);
  expect(transcript[0]?.content, 'the first row must be the first question, unmodified').toContain(tokenA);
  expect(
    transcript[1]?.content,
    'the first answer must still be the answer to the FIRST question — a second turn must not overwrite it',
  ).toContain(tokenA);
  expect(transcript[2]?.content, 'the third row must be the second question').toContain(tokenB);
  // The one assertion the model itself has to satisfy. Both backends this
  // project runs against return the token (the mock echoes its input; a real
  // model is instructed to), and without it "the second answer is non-empty"
  // cannot tell a fresh answer from the first one re-rendered under a new id.
  expect(
    transcript[3]?.content,
    'the second answer must answer the SECOND question, not repeat the first',
  ).toContain(tokenB);
  expect(
    transcript.filter((row) => row.isError).map((row) => row.content.slice(0, 200)),
    'no row may be flagged is_error — a refused turn is stored as an assistant row and renders like an answer',
  ).toEqual([]);
  // Distinct identities, so "four rows" cannot be satisfied by the same group
  // served twice.
  expect(new Set(transcript.map((row) => row.uid)).size, 'every stored row must be its own message group').toBe(4);

  // ── A second, independently computed witness ────────────────────────────
  // `message_count` is a `COUNT(*)` over `chat_message_group` computed by the
  // conversation's own detail query, not by the transcript route. Two turns is
  // four groups.
  const detail = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`,
  );
  expect(detail.status(), 'the conversation must still be readable at its own route').toBe(200);
  const detailBody = (await detail.json()) as { id?: string; message_count?: number };
  expect(detailBody.id, 'the detail route must serve the conversation the send created').toBe(conversationId);
  expect(
    detailBody.message_count,
    'the conversation must count both turns — 2 questions + 2 replies',
  ).toBe(4);

  // ── The fork guard, now that both turns have been observed ──────────────
  expect(
    conversationCreates,
    'the second turn must continue the first conversation, not open another one',
  ).toHaveLength(1);
});
