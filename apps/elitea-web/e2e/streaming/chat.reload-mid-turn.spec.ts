/**
 * A reload while a turn is still streaming (#6654).
 *
 * The page seeds the transcript from the conversation read, whose last row is
 * still `is_streaming` and names its execution in `task_id`. Before the fix,
 * nothing observed that execution again: the answer stayed a "..."
 * placeholder, and the seed rule kept the placeholder over later refetches.
 * Now the page replays the durable execution log from cursor 0.
 *
 * The turn is the mock's `[[mock:slow]]` script (about 20 s of open stream,
 * ending in `MOCKSTREAMEND`), so the reload lands mid-turn by construction.
 * The store, not the screen, proves the turn was still running at reload time.
 */
import { expect, test } from '@playwright/test';
import type { Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { readStoredAssistantAnswer } from '../fixtures/api';

const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const EVENTS_RE = /\/executions\/(\d+)\/[^/]+\/events/;

const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'E2E-MOCK-MODEL';
const SLOW_MARKER = '[[mock:slow]]';
const SLOW_PARTIAL_MARK = 'slow-005';
const SLOW_SENTINEL = 'MOCKSTREAMEND';

async function pickMockModel(page: Page): Promise<void> {
  await page.getByTestId('model-selector-button').click();
  const option = page.getByRole('menuitem').filter({ hasText: MOCK_MODEL }).first();
  await expect(option, `the mock model ${MOCK_MODEL} must be offered`).toBeVisible({ timeout: 20_000 });
  await option.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MOCK_MODEL, { timeout: 10_000 });
}

test('#6654: a reload mid-turn shows the final answer without another reload', async ({ page }) => {
  test.setTimeout(180_000);
  const stamp = Date.now();
  const prompt = `autotest reload-mid-turn-${stamp} ${SLOW_MARKER}`;

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
  await pickMockModel(page);
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 15_000 });
  await input.fill(prompt);

  const created = page.waitForResponse(
    (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', { timeout: 30_000 });
  await page.getByTestId('chat-send-button').click();

  const createdResponse = await created;
  expect(createdResponse.status()).toBe(201);
  const projectId = CONVERSATIONS_RE.exec(new URL(createdResponse.url()).pathname)?.[1] ?? '';
  const conversationId = ((await createdResponse.json()) as { id?: string }).id ?? '';
  expect(conversationId).toMatch(/^\d+$/);
  expect((await started).status(), 'the turn must be admitted').toBe(200);
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 60_000 });

  await expect
    .poll(async () => (await readStoredAssistantAnswer(page, projectId, conversationId)).content, {
      timeout: 60_000,
      message: 'the answer never reached the partial mark, so the reload would not land mid-turn',
    })
    .toContain(SLOW_PARTIAL_MARK);
  const stored = await readStoredAssistantAnswer(page, projectId, conversationId);
  expect(stored.content, 'the turn must still be running when the page reloads').not.toContain(SLOW_SENTINEL);

  // The reloaded page must observe the execution again.
  const reattached = page.waitForResponse((r) => EVENTS_RE.test(r.url()), { timeout: 30_000 });
  await page.reload();
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
  expect((await reattached).status(), 'the reloaded page must reopen the execution stream').toBe(200);

  // The final answer arrives on the SAME page load, once, in full.
  const answer = page.getByText(SLOW_SENTINEL, { exact: false });
  await expect(answer.first(), 'the reloaded page must show the finished answer').toBeVisible({ timeout: 60_000 });
  await expect(answer, 'the replay must not draw the answer twice').toHaveCount(1);
  await expect(page.getByText(`slow-001 slow-002`).first()).toBeVisible();
  await expect(page.getByText(/^\.\.\.$/), 'no streaming placeholder is left behind').toHaveCount(0);
});
