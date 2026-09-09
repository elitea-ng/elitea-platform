/**
 * Clearing a transcript that really exists.
 *
 * Ported by use case from the legacy private suite: "a Clear-chat-history
 * control empties the whole conversation after a confirmation, and the
 * emptying survives a reload" (TC-CHAT-013).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS HALF IS ON `chat-stream` AND NOT WITH THE OTHER CHAT JOURNEYS
 * ─────────────────────────────────────────────────────────────────────────────
 * The same reason `chat.message-delete.spec.ts` next door is: clearing a
 * conversation is a server operation on rows.
 * `deploy/docker-compose.e2e-standalone.yml` — where `journeys/chat/
 * chat.clearHistory.spec.ts` runs — mounts no runtime plane and therefore
 * persists no `chat_message_group` at all, so the bubbles there are the app's
 * own optimistic copies and "the stored transcript is empty afterwards" is
 * true before the clear as well. That file asserts what IS discriminating in
 * that stack (the control exists, the confirmation gates it, and the confirmed
 * clear reaches the plural delete-all route and is accepted). This file
 * asserts the half only a real turn can state: the rows were there, and
 * afterwards they are not.
 *
 * `ConversationsRepo.DeleteMessages` deletes the group graph
 * (`chat_message_group` -> `chat_message_items` -> `chat_messages_text`), and
 * used to delete from a `chat_messages` table no migration creates — so a
 * clear answered 500 on a clean install and silently cleared nothing where
 * pylon had left the legacy table behind (#599). "The bubbles disappeared" is
 * therefore not evidence of anything on its own: `clearChat` empties the local
 * history whatever the server did. The store, read back after a reload, is.
 *
 * The conversation itself must SURVIVE. The clear-all route
 * (`/messages/prompt_lib/{projectID}/{conversationID}`) and the delete-
 * conversation route (`/conversation/prompt_lib/{projectID}/{id}`) differ by
 * one path segment, and a clear wired to the second empties the screen just as
 * convincingly while destroying the conversation.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { expectStoredAssistantAnswer, readStoredMessageGroups } from '../fixtures/api';

/** The create and turn-start routes, matched on the pathname so a query string cannot break them. */
const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
/**
 * The clear-all route: PLURAL `messages`, and the conversation as the last
 * segment. Anchored so the singular per-message delete beside it
 * (`/message/prompt_lib/{projectID}/{messageID}`) can never satisfy it — a
 * clear that deleted one message would otherwise read as a working clear.
 */
const CLEAR_RE = /\/elitea_core\/messages\/prompt_lib\/\d+\/[0-9a-zA-Z-]+$/;

/** The model the standalone stack seeds; overridable for the real-model lane. */
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

/** `ChatBox` names the conversation after the FIRST question, truncated to 50 chars. */
const MAX_NAME = 50;

function uniqueToken(tag: string): string {
  return `AUTOTEST${tag}${Date.now().toString(36).toUpperCase()}`;
}

test('clearing the chat empties the stored transcript and keeps the conversation', async ({ page }) => {
  // A whole agent turn — conversation create, admission, dispatch to the
  // worker, a model call and the stream back — plus the clear and a reload.
  test.setTimeout(300_000);

  const token = uniqueToken('CLR');
  const prompt = `autotest echo exactly: ${token}`;
  expect(prompt.length, 'the prompt must fit the 50-char conversation-name truncation').toBeLessThanOrEqual(MAX_NAME);

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  // The seeded model is not decoration: an ad-hoc turn resolves against a
  // `dummy` participant carrying the model, and the start route reads
  // `llm_settings.model_name`. With nothing selected the send is refused 400
  // before it reaches the worker.
  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('menuitem').filter({ hasText: MODEL_NAME }).first();
  await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
  await modelOption.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

  // ── the turn whose rows this journey clears ─────────────────────────────
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
  expect(projectId, 'the conversation must belong to a project').not.toBe('');
  const createdBody = (await createdResponse.json()) as { id?: string; name?: string };
  const conversationId = createdBody.id ?? '';
  expect(conversationId).toMatch(/^\d+$/);

  const startResponse = await started;
  expect(startResponse.status(), `the turn was refused: ${(await startResponse.text()).slice(0, 300)}`).toBe(200);

  // Waited on the STORE, and on this run's own token: a refused turn is stored
  // as an assistant row too, so "an answer appeared" cannot tell the two apart.
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 120_000,
    message: 'the answer was never stored, so there is no transcript to clear',
    contains: token,
  });

  // `handleConversationCreated` navigates here; waited for explicitly, because
  // interacting mid-navigation drives a component about to be replaced.
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 60_000 });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  const before = await readStoredMessageGroups(page, projectId, conversationId);
  expect(before.length, 'a completed turn stores the question and its answer').toBe(2);

  // ── the clear ───────────────────────────────────────────────────────────
  const clear = page.getByTestId('chat-clear-history');
  await expect(clear, 'the conversation surface must offer a clear-history control').toBeVisible({ timeout: 30_000 });
  await expect(clear, 'a stored transcript makes the control offerable').toBeEnabled({ timeout: 30_000 });
  await clear.click();

  const dialog = page.getByTestId('chat-delete-confirm-dialog');
  await expect(dialog).toBeVisible({ timeout: 15_000 });
  await expect(dialog).toContainText('Clear chat');

  // Nothing may have been cleared yet. Counted from here, so a request the
  // opening click issued would already have been recorded.
  const clears: number[] = [];
  page.on('response', (response) => {
    if (CLEAR_RE.test(new URL(response.url()).pathname) && response.request().method() === 'DELETE') {
      clears.push(response.status());
    }
  });
  await page.evaluate(() => undefined);
  expect(clears, 'opening the confirmation must not clear anything').toEqual([]);

  const cleared = page.waitForResponse(
    (r) => CLEAR_RE.test(new URL(r.url()).pathname) && r.request().method() === 'DELETE',
    { timeout: 45_000 },
  );
  await dialog.getByRole('button', { name: 'Delete', exact: true }).click();
  expect((await cleared).status(), 'the confirmed clear must reach the server').toBe(204);

  // ── the store is what "cleared" means ───────────────────────────────────
  await expect
    .poll(async () => (await readStoredMessageGroups(page, projectId, conversationId)).length, {
      timeout: 45_000,
      message: 'the clear emptied the screen but left the rows — the local reset hides exactly this',
    })
    .toBe(0);

  await expect(page.getByTestId('user-message')).toHaveCount(0);
  await expect(page.getByTestId('application-answer')).toHaveCount(0);

  // …and it stays gone across a reload, which is the reading the local history
  // cannot fake — while the conversation itself is still there to reload.
  await page.reload();
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 30_000 });
  await expect(page.getByText('Something went wrong.')).toHaveCount(0);
  await expect(page.getByTestId('user-message')).toHaveCount(0);
  await expect(page.getByTestId('application-answer')).toHaveCount(0);
  expect(page.url()).toContain(`/app/chat/${conversationId}`);
  expect(
    (await readStoredMessageGroups(page, projectId, conversationId)).length,
    'the conversation must survive its own clear',
  ).toBe(0);
});
