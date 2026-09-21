/**
 * WHAT A REGENERATION TELLS THE PERSON WHO ASKED FOR IT (#939 group 8).
 *
 * `chat.regenerate.spec.ts` owns the STRUCTURE of a regeneration: the same
 * answer row is rewritten, a new `execution_generation` is recorded, no second
 * conversation or start POST appears. This file owns what the source cases ask
 * about instead — the SIGNALS around it:
 *
 *   - a regeneration that SUCCEEDS must raise no error toast (ELITEA-0380);
 *   - the rewritten answer must carry the time it was rewritten, while the
 *     question it answers keeps the time it was asked (ELITEA-0400).
 *
 * They are asserted in SEPARATE tests, each paying its own regeneration, and
 * that is not waste: the second one FAILS (#975 — the answer is not re-timed),
 * and sharing a turn would have taken the first one's proof down with it.
 *
 * ── WHY THE TIMESTAMPS ARE READ FROM THE STORE ───────────────────────────
 *
 * The case describes them as rendered strings ("20 minutes ago" → "just now").
 * A relative label is a function of the clock at render time, so asserting on
 * one measures how long the test itself took. The stored time the renderer
 * derives those strings FROM is the same fact without the clock dependency.
 *
 * And there is only ONE such field: the route's `Message` struct
 * (`internal/api/v2/conversations/handler.go`) serves `created_at` and carries
 * no `updated_at` at all, so `created_at` is the whole of what any renderer
 * can show — which is itself half of why #975 exists.
 *
 * ── THE VARIANTS THIS DOES NOT PAY FOR ───────────────────────────────────
 *
 * ELITEA-0380 is parametrized over three surfaces (chat page, agent page,
 * pipeline page). The claim under test is the CLIENT's handling of a 200 from
 * one route, and the three surfaces mount the same answer card and the same
 * toast layer; three regenerations would cost three model turns to re-prove
 * one branch. The chat page is driven here and the scope is stated rather than
 * implied.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { API_BASE, expectStoredAssistantAnswer } from '../fixtures/api';

/** Matched WITHOUT a project id: the chat driver acts inside its own personal project (#290). */
const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const REGENERATE_RE = /\/elitea_core\/regenerate\/prompt_lib\/(\d+)\/([0-9a-f-]+)$/;

/** The model `seed-llm` seeds; `E2E_CHAT_MODEL` names a real one an operator has instead. */
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

/** `ChatBox` names the conversation after the question, truncated to 50 chars. */
const MAX_NAME = 50;

function uniqueToken(tag: string): string {
  return `${tag}${Date.now().toString(36)}${Math.floor(Math.random() * 46_656).toString(36)}`;
}

/** Every toast currently on screen — the union of the MUI Snackbar/Alert content this app's toast layer renders. */
async function toastTexts(page: Page): Promise<readonly string[]> {
  return page.getByRole('alert').allTextContents();
}

interface StoredRow {
  readonly role: string;
  readonly createdAt: string;
  readonly content: string;
}

/**
 * The transcript with its TIMES — which `readStoredTranscript` does not carry,
 * because no other journey needed them.
 *
 * `created_at` and nothing else: MEASURED, the route's `Message` struct
 * (`internal/api/v2/conversations/handler.go`) serves `created_at` and has no
 * `updated_at` at all, so `created_at` is the only time any renderer can be
 * deriving a message's "when" from.
 *
 * `sort_order=asc`, so `[0]` is the question and `[1]` its answer; the route's
 * documented default is `created_at DESC` (#603) and reading that order as
 * ascending would compare the answer's time against itself.
 */
async function readStoredTimes(page: Page, projectId: string, conversationId: string): Promise<readonly StoredRow[]> {
  const response = await page.request.get(
    `${API_BASE}/elitea_core/messages/prompt_lib/${projectId}/${conversationId}?sort_order=asc&limit=100`,
  );
  expect(response.ok(), `the transcript read answered ${String(response.status())}`).toBe(true);
  const body = (await response.json()) as {
    items?: readonly { role?: string; created_at?: string; content?: string }[];
  };
  return (body.items ?? []).map((item) => ({
    role: item.role ?? '',
    createdAt: String(item.created_at ?? ''),
    content: String(item.content ?? ''),
  }));
}

/**
 * One completed turn, then one SUCCESSFUL regeneration of it.
 *
 * Shared because both tests below need exactly this and neither may inherit
 * the other's conversation: a regeneration is asserted against the transcript
 * as it stood immediately before it, and two tests sharing one would each be
 * measuring the other's leftovers.
 */
async function turnThenRegenerate(page: Page, tag: string): Promise<{
  projectId: string;
  conversationId: string;
  token: string;
  before: readonly StoredRow[];
  toastsBefore: readonly string[];
}> {
  const token = uniqueToken(tag);
  const prompt = `autotest echo exactly: ${token}`;
  expect(prompt.length, 'the prompt must fit the 50-char conversation-name truncation').toBeLessThanOrEqual(MAX_NAME);

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('menuitem').filter({ hasText: MODEL_NAME }).first();
  await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
  await modelOption.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

  const created = page.waitForResponse(
    (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 45_000,
  });

  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  await input.fill(prompt);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();

  const createdResponse = await created;
  expect(createdResponse.status(), 'the send must create a real conversation').toBe(201);
  const projectId = CONVERSATIONS_RE.exec(new URL(createdResponse.url()).pathname)?.[1] ?? '';
  const conversationId = String(((await createdResponse.json()) as { id?: unknown }).id ?? '');
  expect(conversationId).toMatch(/^\d+$/);
  expect((await started).status(), 'the turn to be regenerated was itself refused').toBe(200);

  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 180_000,
    message: 'there is no answer to regenerate — the first turn never stored one',
    ...(process.env['E2E_CHAT_MODEL'] ? {} : { contains: token }),
  });
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 60_000 });
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 120_000 });

  const before = await readStoredTimes(page, projectId, conversationId);
  expect(
    before.map((row) => row.role),
    'the conversation must hold exactly one question and one answer before regenerating',
  ).toEqual(['user', 'assistant']);
  expect(before[0]?.createdAt, 'the question must carry a stored time').not.toBe('');
  expect(before[1]?.createdAt, 'the answer must carry a stored time').not.toBe('');

  // Toasts already on screen are NOT this regeneration's, so the baseline is
  // taken here: the assertion is "the regeneration added no error", never "the
  // session has been silent".
  const toastsBefore = await toastTexts(page);

  // Hovered, not clicked blind: the action row is `visibility: hidden` until
  // its answer block is hovered, and `visibility` takes an element out of the
  // accessibility tree — so a control that exists but never becomes reachable
  // fails here as "not visible" rather than passing on a node no user could
  // press.
  const answerCard = page.getByTestId('application-answer').first();
  await answerCard.getByTestId('skill-test-last-response').hover();
  const regenerate = answerCard.getByRole('button', { name: 'Regenerate' });
  await expect(regenerate, 'a completed answer must offer Regenerate').toBeVisible({ timeout: 20_000 });

  const regenerated = page.waitForResponse(
    (r) => REGENERATE_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST' && r.status() !== 409,
    { timeout: 90_000 },
  );
  await regenerate.click();
  const regenerateResponse = await regenerated;
  expect(
    regenerateResponse.status(),
    `the regeneration was refused, so nothing here can say anything about a SUCCESSFUL one: ${(
      await regenerateResponse.text()
    ).slice(0, 300)}`,
  ).toBe(200);

  // Waited on the STORE: every assertion below is about a finished run.
  // Not on the text or the time — the mock is deterministic, so the
  // regenerated bytes are the same bytes, and the time is #975's subject.
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 180_000,
    message: 'the regenerated answer never landed in the store',
    ...(process.env['E2E_CHAT_MODEL'] ? {} : { contains: token }),
  });

  return { projectId, conversationId, token, before, toastsBefore };
}

/*
 * onetest: ELITEA-0380 (chat-page variant) — a regeneration the server accepted
 * must raise no error toast, and must not disturb the question it answers.
 */
test('a successful regeneration raises no error toast and leaves the question’s own time alone', async ({ page }) => {
  // Two model calls — the turn and its regeneration — plus stack round trips.
  test.setTimeout(420_000);

  const { projectId, conversationId, before, toastsBefore } = await turnThenRegenerate(page, 'rgt');

  // The toast layer renders errors as `role="alert"`. A successful
  // regeneration that still raised one is the defect this case is named for.
  const raised = (await toastTexts(page)).filter((text) => !toastsBefore.includes(text));
  expect(raised, `a successful regeneration raised a toast: ${JSON.stringify(raised)}`).toEqual([]);

  const after = await readStoredTimes(page, projectId, conversationId);
  expect(
    after.map((row) => row.role),
    'regenerating must not add or remove a row',
  ).toEqual(['user', 'assistant']);
  // The half of ELITEA-0400 that HOLDS: a regeneration must not re-stamp the
  // whole exchange. Asserted here, with the passing half, so the gap below
  // cannot swallow it.
  expect(
    after[0]?.createdAt,
    'the question keeps the time it was asked',
  ).toBe(before[0]?.createdAt);
});

/*
 * onetest: ELITEA-0400 — the regenerated answer must carry the time it was
 * regenerated, so the transcript stops claiming the new text is as old as the
 * text it replaced.
 *
 * IT DOES NOT. The regeneration rewrites the answer row in place and leaves
 * `created_at` exactly as it was — measured: the same millisecond, before and
 * after — and the route serves no `updated_at` at all, so there is no other
 * field a renderer could be reading. The person sees fresh text under the old
 * timestamp. See #975.
 */
test('a regenerated answer carries the time it was regenerated', async ({ page }) => {
  test.fail(
    true,
    '#975: product gap — regeneration rewrites the answer row without touching `created_at`, and the messages route serves no `updated_at`, so a regenerated answer keeps the original time',
  );
  test.setTimeout(420_000);

  const { projectId, conversationId, before } = await turnThenRegenerate(page, 'rgs');

  const after = await readStoredTimes(page, projectId, conversationId);
  expect(
    Date.parse(after[1]?.createdAt ?? ''),
    'the regenerated answer must be stamped with the time it was regenerated',
  ).toBeGreaterThan(Date.parse(before[1]?.createdAt ?? ''));
});
