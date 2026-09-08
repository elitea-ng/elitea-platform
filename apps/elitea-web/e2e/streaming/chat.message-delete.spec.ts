/**
 * Deleting a message from a transcript that really exists.
 *
 * Ported by use case from the legacy public suite:
 *  - `qa/elitea-testing-public/automation/tests/ui/chat/test_chat_interface.py::TestMessageActions::test_delete_message`
 *    (TC-CHAT-009)
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS ONE IS ON `chat-stream` AND NOT WITH THE OTHER CHAT JOURNEYS
 * ─────────────────────────────────────────────────────────────────────────────
 * Deleting a message is a server operation on a row, not a screen state:
 * `ChatMessageList` offers Delete on the LAST message only, `createDeleteAnswer`
 * issues `DELETE /elitea_core/messages/prompt_lib/{p}/{c}/{groupUID}`, and
 * `ConversationsRepo.DeleteMessage` refuses a uuid with no `chat_message_group`
 * row with a 404. `docker-compose.e2e-standalone.yml` — where every other chat
 * journey runs — persists no message group at all: it has no runtime plane, so
 * the only bubble on screen there is an optimistic client-side one whose id
 * names nothing in the database, and the delete could only ever fail. A journey
 * written there would be asserting the absence of a stack, not the presence of
 * a feature. Here a real turn writes the rows the delete is about.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THE ASSERTIONS HAVE TO SAY, BEYOND "A BUBBLE DISAPPEARED"
 * ─────────────────────────────────────────────────────────────────────────────
 *  1. THE PAIR GOES, NOT THE ANSWER ALONE. The repository deletes the named
 *     group AND the group it replies to, and the confirmation copy says so
 *     ("This also removes the question it answers"). A delete that removed only
 *     the answer would leave a question with no reply in the model's context —
 *     the outcome the pairing exists to prevent — while looking correct on
 *     screen for exactly as long as nobody reloaded.
 *  2. THE SCREEN AND THE STORE MUST AGREE. `createDeleteAnswer` falls back to
 *     removing the requested id from the local history when the response's
 *     `deleted` array is empty, so a 404'd or no-op delete still clears the
 *     bubble. The bubble disappearing is therefore not evidence of anything;
 *     the stored transcript is.
 *  3. THE CONFIRMATION IS REQUIRED. `useDeleteMessageAlert` opens a dialog and
 *     only `confirmDelete` calls the mutation. A control that deleted straight
 *     out of its `onClick` would satisfy every other assertion here.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { expectStoredAssistantAnswer, readStoredMessageGroups } from '../fixtures/api';

/** The create and turn-start routes, matched on the pathname so a query string cannot break them. */
const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
/** The per-message delete route this journey exists to reach. */
const DELETE_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+\/[0-9a-f-]+/;

/** The model the standalone stack seeds; overridable for the real-model lane. */
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

/** `ChatBox` names the conversation after the FIRST question, truncated to 50 chars. */
const MAX_NAME = 50;

function uniqueToken(tag: string): string {
  return `AUTOTEST${tag}${Date.now().toString(36).toUpperCase()}`;
}

test('deleting the last message removes the whole turn, on screen and in the store', async ({ page }) => {
  // One whole agent turn — conversation create, admission, dispatch to the
  // worker, a model call and the stream back — plus the delete. Every wait
  // below is bounded well under this, so a real hang fails on its own step.
  test.setTimeout(300_000);

  const token = uniqueToken('DEL');
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

  // ── the turn whose rows this journey deletes ────────────────────────────
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
  const conversationId = ((await createdResponse.json()) as { id?: string }).id ?? '';
  expect(conversationId).toMatch(/^\d+$/);

  const startResponse = await started;
  expect(startResponse.status(), `the turn was refused: ${(await startResponse.text()).slice(0, 300)}`).toBe(200);

  // Waited on the STORE, and on this run's own token: a refused turn is stored
  // as an assistant row too, so "an answer appeared" cannot tell the two apart.
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 120_000,
    message: 'the answer was never stored, so there is no turn to delete',
    contains: token,
  });

  // `handleConversationCreated` navigates here; waited for explicitly, because
  // interacting mid-navigation drives a component about to be replaced.
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 60_000 });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  // The pair the delete is about, as the store holds it: the question and the
  // answer that replies to it.
  const before = await readStoredMessageGroups(page, projectId, conversationId);
  expect(before.length, 'a completed turn stores the question and its answer').toBe(2);

  // ── the delete ──────────────────────────────────────────────────────────
  const answer = page.getByTestId('application-answer').last();
  await expect(answer).toBeVisible({ timeout: 30_000 });
  await answer.hover();
  const remove = answer.getByRole('button', { name: 'Delete', exact: true });
  await expect(remove, 'the last message must offer Delete').toBeVisible({ timeout: 15_000 });
  await remove.click();

  // The confirmation is a step, not decoration — and it names what really goes.
  const dialog = page.getByTestId('chat-delete-confirm-dialog');
  await expect(dialog).toBeVisible({ timeout: 15_000 });
  await expect(dialog).toContainText('This also removes the question it answers');

  // Nothing may have been deleted yet. Counted from here, so a request the
  // opening click issued would already have been recorded.
  const deletes: number[] = [];
  page.on('response', (response) => {
    if (DELETE_RE.test(new URL(response.url()).pathname) && response.request().method() === 'DELETE') {
      deletes.push(response.status());
    }
  });
  await page.evaluate(() => undefined);
  expect(deletes, 'opening the confirmation must not delete anything').toEqual([]);

  const deleted = page.waitForResponse(
    (r) => DELETE_RE.test(new URL(r.url()).pathname) && r.request().method() === 'DELETE',
    { timeout: 45_000 },
  );
  await dialog.getByRole('button', { name: 'Delete', exact: true }).click();
  expect((await deleted).status(), 'the confirmed delete must reach the server').toBe(200);

  // ── the store is what "deleted" means ───────────────────────────────────
  await expect
    .poll(async () => (await readStoredMessageGroups(page, projectId, conversationId)).length, {
      timeout: 45_000,
      message: 'the delete removed the bubble but left the rows — the local fallback hides exactly this',
    })
    .toBe(0);

  // The whole turn, not the answer alone: the question went with it.
  await expect(page.getByTestId('application-answer')).toHaveCount(0);
  await expect(page.getByTestId('user-message')).toHaveCount(0);

  // …and it stays gone across a reload, which is the reading the local
  // history cannot fake.
  await page.reload();
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 30_000 });
  await expect(page.getByTestId('user-message')).toHaveCount(0);
  await expect(page.getByTestId('application-answer')).toHaveCount(0);
});
