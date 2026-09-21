/**
 * EDITING THE QUESTION YOU ALREADY ASKED (#939 group 8).
 *
 * Five cases about one control — the pencil on a user message:
 *
 *   * ELITEA-0548 — it is on the LAST user message and no other;
 *   * ELITEA-0537 — older user messages and assistant messages never have it;
 *   * ELITEA-0547 — its edge cases: it exists on a single-message
 *     conversation, it carries a tooltip, and it is gone while a turn streams;
 *   * ELITEA-0540 — saving WITHOUT changing the text still re-runs the turn,
 *     i.e. the control doubles as a retry;
 *   * ELITEA-0545 — the text really is editable: modifications, special
 *     characters and a long message all survive the round trip.
 *
 * The first three hold. The last two DO NOT (#980): an unchanged save cannot
 * be pressed, and a changed one dispatches no request at all — so the gating
 * this control advertises is correct and the control itself does nothing.
 *
 * All five need a PERSISTED conversation with real exchanges in it — the
 * control is gated on `index === lastUserMessageIndex` over the rendered
 * history (`ChatMessageList.tsx`), so a conversation with nothing in it
 * proves none of them. That is why these live in the streaming lane and not
 * in `journeys/**`, which has no worker to produce an answer at all.
 *
 * ── THE GATE, READ FROM THE PRODUCT RATHER THAN GUESSED ──────────────────
 *
 *   isEligibleForEdit =
 *     isUser && index === lastUserMessageIndex && !isStreaming &&
 *     isOwnMessage(userId, message.userId)
 *
 * Four conditions, and the cases between them name three. The assertions
 * below are written against that expression so a change to it fails here
 * rather than in whichever journey happens to hover a pencil next.
 *
 * ── WHAT THE SUBMIT ACTUALLY DOES ────────────────────────────────────────
 *
 * `handleSubmitEditedMessage` does not PATCH the message: it rewrites the row
 * locally and then calls `regenerateAnswer(answerId, updatedItems)` — the
 * same route `chat.regenerate.spec.ts` owns. So "save and apply" IS a
 * regeneration carrying new text, which is exactly why ELITEA-0540's
 * unchanged-save is a retry rather than a no-op, and why the assertions below
 * read the regenerate route and the stored transcript rather than a save
 * endpoint that does not exist.
 */
import { expect, test, type Locator, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  createAgentWithVersion,
  deleteAgent,
  expectStoredAssistantAnswer,
  fillComposer,
  readCallerPersonalProjectId,
  readStoredTranscript,
} from '../fixtures/api';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const REGENERATE_RE = /\/elitea_core\/regenerate\/prompt_lib\/(\d+)\/([0-9a-f-]+)$/;
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

/** The product's own label for the control — tooltip and accessible name are the same string. */
const EDIT_LABEL = 'Edit the message and regenerate answer';

/** The editor's commit button. Matched by its exact label so `Cancel` can never be clicked by accident. */
const SAVE_LABEL = 'Save and apply';

function marker(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}${Date.now() % 1_000_000}${Math.floor(Math.random() * 46_656).toString(36)}`;
}

/** Every edit control currently rendered, across every message. */
function editControls(page: Page): Locator {
  return page.getByRole('button', { name: EDIT_LABEL });
}

/** The user-message rows, in document order. */
function userMessages(page: Page): Locator {
  return page.getByTestId('user-message');
}

async function openAgentChat(page: Page, agentId: string): Promise<string> {
  await page.goto(`${BASE_URL}/app/agents/all/${agentId}`);
  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const conversationId = String(((await (await conversationCreated).json()) as { id?: unknown }).id ?? '');
  expect(conversationId, 'the Chat button must create a conversation').not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 45_000 });
  return conversationId;
}

/** Send one message and wait for its answer to be STORED, so the next assertion sees a settled history. */
async function completeTurn(page: Page, projectId: string, conversationId: string, text: string): Promise<void> {
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  const sendButton = await fillComposer(page, text);
  await sendButton.click();
  expect((await started).status(), 'the turn was refused').toBe(200);
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 180_000,
    contains: text.slice(-24),
    message: `the turn "${text.slice(0, 40)}" stored no answer`,
  });
}

async function createChatAgent(page: Page, projectId: string, name: string): Promise<{ id: string }> {
  const created = await createAgentWithVersion(
    page.request,
    name,
    {
      instructions: 'You are an edit-message fixture. Echo the user back briefly.',
      welcomeMessage: 'Ask me something.',
      conversationStarters: ['Hello.'],
      model: { modelName: MOCK_MODEL },
      meta: { step_limit: 25, internal_tools: [] },
    },
    projectId,
    `${AUTOTEST_PREFIX}edit-message fixture`,
  );
  return { id: created.id };
}

/*
 * onetest: ELITEA-0548 (the control is on the last user message only and moves
 * when a newer one arrives), ELITEA-0537 (older user messages and assistant
 * messages are not editable) and ELITEA-0547 (its edge cases: present on a
 * single-message conversation, carrying a tooltip).
 */
test('the edit control belongs to the last user message alone, and follows it', async ({ page }) => {
  test.setTimeout(420_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'the chat persona must own a personal project (#290)').not.toBe('');

  let agentId = '';
  try {
    const agent = await createChatAgent(page, projectId, `${AUTOTEST_PREFIX}edit-${String(Date.now()).slice(-7)}`);
    agentId = agent.id;
    const conversationId = await openAgentChat(page, agent.id);

    // ── ELITEA-0547: a conversation with ONE exchange ────────────────────
    const first = marker('q1');
    await completeTurn(page, projectId, conversationId, `Say this back: ${first}`);
    await expect(userMessages(page)).toHaveCount(1, { timeout: 30_000 });
    await expect(
      editControls(page),
      'a single-message conversation must still offer the edit control',
    ).toHaveCount(1, { timeout: 30_000 });
    // The tooltip IS the accessible name here — the product gives the button
    // both from one string — so locating it by role and name has already
    // asserted the case's tooltip half.
    await expect(editControls(page).first()).toBeVisible();

    // ── ELITEA-0548 / 0537: a second exchange moves it ───────────────────
    const second = marker('q2');
    await completeTurn(page, projectId, conversationId, `Say this back: ${second}`);
    await expect(userMessages(page)).toHaveCount(2, { timeout: 30_000 });

    // STILL exactly one control in the whole conversation. This is the
    // assertion that catches "every user message got a pencil", which is what
    // a gate that dropped `index === lastUserMessageIndex` would produce.
    await expect(
      editControls(page),
      'exactly one message may be editable, however many exchanges there are',
    ).toHaveCount(1, { timeout: 30_000 });

    // And it is on the LAST user message, not the first. Scoped to the row,
    // so this cannot pass on a control that merely exists somewhere on screen.
    await expect(
      userMessages(page).nth(1).getByRole('button', { name: EDIT_LABEL }),
      'the control must be on the newest user message',
    ).toHaveCount(1);
    await expect(
      userMessages(page).nth(0).getByRole('button', { name: EDIT_LABEL }),
      'the older user message must no longer be editable',
    ).toHaveCount(0);

    // ELITEA-0537's other half: no ASSISTANT message carries one.
    const answers = page.getByTestId('application-answer');
    expect(await answers.count(), 'the conversation must hold answers to check').toBeGreaterThan(0);
    for (let index = 0; index < (await answers.count()); index += 1) {
      await expect(
        answers.nth(index).getByRole('button', { name: EDIT_LABEL }),
        'an assistant message is never editable',
      ).toHaveCount(0);
    }
  } finally {
    if (agentId !== '') await deleteAgent(page.request, agentId).catch(() => undefined);
  }
});

/*
 * onetest: ELITEA-0540 — "Save and apply" without changing the text must still
 * re-run the turn: the control is a retry as well as an edit.
 *
 * IT CANNOT BE PRESSED. `UserMessage.tsx` disables the button on exactly that
 * state:
 *
 *   disabled={value === resolvedContent || !value.trim()}
 *
 * so an unchanged save is not merely ignored — it is unreachable, and the
 * retry the case describes does not exist. See #980.
 */
test('saving an edited question without changing it re-runs the turn', async ({ page }) => {
  test.setTimeout(420_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'the chat persona must own a personal project (#290)').not.toBe('');

  let agentId = '';
  try {
    const agent = await createChatAgent(page, projectId, `${AUTOTEST_PREFIX}edit2-${String(Date.now()).slice(-7)}`);
    agentId = agent.id;
    const conversationId = await openAgentChat(page, agent.id);

    const original = marker('orig');
    await completeTurn(page, projectId, conversationId, `Say this back: ${original}`);

    await editControls(page).first().click();
    const row = userMessages(page).last();
    const editor = row.getByRole('textbox');
    await expect(editor, 'the edit control must open an editor on the message').toBeVisible({ timeout: 15_000 });
    await expect(editor).toHaveValue(new RegExp(original));

    // Nothing is typed: this IS the case — reopen and confirm.
    const save = row.getByRole('button', { name: SAVE_LABEL });
    await expect(
      save,
      'an unchanged question must still be re-runnable — the control is a retry as well as an edit',
    ).toBeEnabled({ timeout: 10_000 });

    const retried = page.waitForResponse(
      (r) => REGENERATE_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST' && r.status() !== 409,
      { timeout: 90_000 },
    );
    await save.click();
    expect((await retried).status(), 'the unchanged save must re-run the turn').toBe(200);
  } finally {
    if (agentId !== '') await deleteAgent(page.request, agentId).catch(() => undefined);
  }
});

/*
 * onetest: ELITEA-0545 — the text really is editable: a modification carrying
 * special characters round-trips into the STORED question and re-runs the
 * turn.
 *
 * IT USED NOT TO LEAVE THE BROWSER at all: the editor opened, accepted the
 * text, the submit closed it, and ZERO requests were made — `handleSubmit`
 * built an EMPTY `updatedItems` for a message carrying no stored question item
 * and `handleSubmitEditedMessage` returned silently. That half is fixed (issue
 * 980): the edit is dispatched now.
 *
 * WHAT IS LEFT IS SERVER-SIDE, and is why this case stays marked. The
 * regeneration contract refuses a non-empty `updated_items` outright
 * (`!emptyJSONArray(body.UpdatedItems)`, internal/api/v2/agentexecution/
 * route.go) and nothing on the server rewrites the stored question's text, so
 * an edited question 400s instead of re-running. Accepting the field, writing
 * the new text onto the question group inside the admission transaction, and
 * running the turn from it is a feature, not a wiring fix.
 */
test('an edited question is stored as written and re-runs the turn', async ({ page }) => {
  test.fail(
    true,
    'issue 980 (deferred half): the client now dispatches the edit, but the REGENERATION CONTRACT refuses it — `!emptyJSONArray(body.UpdatedItems)` in internal/api/v2/agentexecution/route.go answers 400 for any non-empty `updated_items`, and nothing on the server rewrites the stored question. Server-side feature, not a wiring bug.',
  );
  test.setTimeout(480_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'the chat persona must own a personal project (#290)').not.toBe('');

  let agentId = '';
  try {
    const agent = await createChatAgent(page, projectId, `${AUTOTEST_PREFIX}edit3-${String(Date.now()).slice(-7)}`);
    agentId = agent.id;
    const conversationId = await openAgentChat(page, agent.id);

    const original = marker('orig');
    await completeTurn(page, projectId, conversationId, `Say this back: ${original}`);

    // Characters that are easy to mangle on the way through a JSON body, a
    // JSONB column and a renderer — and a long tail, which is the case's
    // "long messages" half.
    const rewritten = `${marker('edit')} — "quoted", <angled>, 100% & unicode ünïcøde ${'x'.repeat(400)}`;
    // NO ROUTE WAIT, deliberately. `handleSubmitEditedMessage` branches — it
    // REGENERATES when it can find the answer whose `questionId` is this
    // message and otherwise SENDS the edited text as a new question — and
    // arming a `waitForResponse` before the click made this test depend on
    // guessing which branch ran, and on winning a race against it. Which
    // route it took is the product's business; that the EDIT LANDED is this
    // test's, and the store below is where that is decided.
    // Every POST the submit produces, recorded so a failure can say whether
    // the client sent NOTHING or sent something the store then ignored — two
    // different defects that look identical from the transcript alone.
    const posted: string[] = [];
    page.on('response', (response) => {
      if (response.request().method() !== 'POST') return;
      const path = new URL(response.url()).pathname;
      if (path.includes('/elitea_core/')) posted.push(`${String(response.status())} ${path}`);
    });

    await editControls(page).first().click();
    const row = userMessages(page).last();
    const editor = row.getByRole('textbox');
    await expect(editor).toBeVisible({ timeout: 15_000 });
    await editor.fill(`Say this back: ${rewritten}`);
    const save = row.getByRole('button', { name: SAVE_LABEL });
    await expect(save, 'a changed question must be saveable').toBeEnabled({ timeout: 10_000 });
    await save.click();
    // The editor closes on submit, which is the client's acknowledgement that
    // it accepted the edit at all.
    await expect(editor, 'the editor must close once the edit is submitted').toBeHidden({ timeout: 30_000 });

    // FIRST: did the edit leave the browser at all? This separates "the
    // client sent nothing" from "the client sent something the store ignored"
    // — two different defects that look identical in the transcript.
    await expect
      .poll(async () => posted.length, { timeout: 30_000, message: 'the submit produced no POST at all' })
      .toBeGreaterThan(0);

    // THE STORE, not the bubble: the question row itself must carry the new
    // text, special characters intact. A client that rewrote only its own
    // state would leave the transcript holding the original.
    await expect
      .poll(
        async () => (await readStoredTranscript(page, projectId, conversationId))[0]?.content ?? '',
        { timeout: 180_000, message: 'the edited question never reached the stored transcript' },
      )
      .toContain(rewritten);
  } finally {
    if (agentId !== '') await deleteAgent(page.request, agentId).catch(() => undefined);
  }
});
