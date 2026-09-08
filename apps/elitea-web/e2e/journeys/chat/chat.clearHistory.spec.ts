/**
 * Emptying a conversation from the chat surface.
 *
 * Ported by use case from the legacy private suite: "a Clear-chat-history
 * control empties the whole conversation after a confirmation, and the
 * emptying survives a reload" (TC-CHAT-013).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS USE CASE HAD NO JOURNEY BEFORE
 * ─────────────────────────────────────────────────────────────────────────────
 * Because the control did not exist. Every part of the mechanism did:
 * `ChatBox` exposes `onClear` on its imperative handle, `handleClear` opens
 * the delete-all confirmation, the `ALL_MESSAGES` sentinel routes the confirm
 * to `clearChat`, and `clearChat` issues
 * `DELETE /elitea_core/messages/prompt_lib/{projectId}/{conversationId}`. The
 * ONLY caller of that handle was the pipeline editor's test-chat panel, so on
 * `/chat` the whole chain was unreachable: a person could empty a
 * conversation one message at a time, or delete the conversation. Two further
 * seams were dead with it — `NewChatInputSlots` had no `clearChat` slot, and
 * `resolveSlots` rebuilds that bundle key by key, so a slot missing from its
 * list is dropped silently between the composition root and the footer.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS STACK CAN AND CANNOT SAY, MEASURED
 * ─────────────────────────────────────────────────────────────────────────────
 * `deploy/docker-compose.e2e-standalone.yml` mounts NO runtime plane, so
 * nothing here can persist a `chat_message_group` — the agent-start route
 * answers 405, and `chat.management.spec.ts`'s module header records the same
 * measurement. The bubbles below are the app's own optimistic copies of what
 * the person typed, kept on screen by `historyAfterFailedTurn` when the turn
 * is refused (`chat.messageActions.spec.ts` asserts that behaviour directly).
 *
 * Two consequences, both faced rather than papered over:
 *
 *  1. "The server no longer holds the messages" is NOT what the read-back at
 *     the end proves here, because the server never held them. What it does
 *     prove is that the clear did not damage the conversation: it still
 *     resolves, and its transcript is empty. The half this stack cannot
 *     reach — a stored transcript really being deleted — is asserted where a
 *     real turn writes real rows, in `e2e/streaming/chat.clearHistory.spec.ts`,
 *     for the same reason `streaming/chat.message-delete.spec.ts` lives there.
 *  2. The DISCRIMINATING assertion is therefore the request itself: the
 *     confirmed clear must reach the PLURAL delete-all route for this
 *     conversation and be accepted. A control that only emptied the local
 *     history — which is exactly what the agents-page branch of `handleClear`
 *     does, and what a mis-wired chat surface would inherit — satisfies every
 *     screen assertion below and fails that one.
 *
 * Both halves of the confirmation are exercised: Cancel must keep the
 * transcript AND issue no request, because "confirm" and "cancel" sit one row
 * apart on the same dialog and a Cancel wired to the same handler looks
 * correct until somebody cancels.
 *
 * Everything this file creates carries the `autotest_` prefix and this file's
 * own `-clear` tag, and the conversation is deleted on the way out through
 * `page.request`, which shares the browser context's session.
 */
import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, deleteConversation } from '../../fixtures/api';

/** Every entity this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-clear';

const CONVERSATIONS_PATH = `/elitea_core/conversations/prompt_lib/${DEFAULT_PROJECT_ID}`;
const CONVERSATION_PATH = `/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;
const MESSAGES_PATH = `/elitea_core/messages/prompt_lib/${DEFAULT_PROJECT_ID}`;

/** `ChatBox` names a conversation after its first question, truncated to 50 chars. */
const MAX_NAME = 50;

function uniqueMessage(tag: string): string {
  const text = `${AUTOTEST_PREFIX}${tag}-${Date.now()}${SUFFIX}`;
  expect(text.length, 'the message must fit the 50-char name truncation').toBeLessThanOrEqual(MAX_NAME);
  return text;
}

/**
 * Types one message and sends it, waiting for the transcript to grow.
 *
 * The bubble count is what is awaited, not a spinner: the turn itself is
 * refused in this stack, so the only observable outcome of a send is the
 * question the composer moved onto the transcript.
 */
async function sendMessage(page: Page, text: string, expectedCount: number): Promise<void> {
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  await input.fill(text);
  const send = page.getByTestId('chat-send-button');
  await expect(send).toBeEnabled({ timeout: 10_000 });
  await send.click();
  await expect(page.getByTestId('user-message')).toHaveCount(expectedCount, { timeout: 30_000 });
  await expect(input).toHaveValue('');
}

/** The conversation's stored transcript, as the server holds it right now. */
async function storedGroupCount(page: Page, conversationId: string): Promise<number> {
  // `messages_limit` is required for the handler to embed `message_groups` at
  // all — without it a well-formed 200 carries none, which reads as "empty"
  // whatever the conversation holds.
  const response = await page.request.get(
    `${API_BASE}${CONVERSATION_PATH}/${conversationId}?messages_limit=50&sort_order=asc`,
  );
  expect(response.status(), 'the conversation must still resolve after a clear').toBe(200);
  const body = (await response.json()) as { message_groups?: readonly unknown[] };
  return (body.message_groups ?? []).length;
}

test('the chat surface clears its whole transcript, and only after the confirmation', async ({ page }) => {
  // Two sends, each refused by a stack with no worker, plus a reload.
  test.setTimeout(180_000);

  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  // Nothing to clear yet, and the control says so rather than hiding: an
  // affordance that disappeared on an empty chat is indistinguishable from
  // the unwired state this journey exists for.
  const clear = page.getByTestId('chat-clear-history');
  await expect(clear, 'the conversation surface must offer a clear-history control').toBeVisible({ timeout: 20_000 });
  await expect(clear).toBeDisabled();

  const first = uniqueMessage('c1');
  const created = page.waitForResponse(
    (response) => response.url().includes(CONVERSATIONS_PATH) && response.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await sendMessage(page, first, 1);
  const conversationId = ((await (await created).json()) as { id?: string }).id as string;
  expect(conversationId, 'the first send must create a real conversation').toMatch(/^\d+$/);

  await sendMessage(page, uniqueMessage('c2'), 2);
  await expect(clear, 'a transcript with messages makes the control offerable').toBeEnabled({ timeout: 20_000 });
  await checkA11y(page);

  // ── the cancel path ───────────────────────────────────────────────────────
  // Counted from here, so a request the ARMING click itself issued is already
  // recorded by the time it is read.
  const deletes: string[] = [];
  page.on('request', (request) => {
    if (request.method() === 'DELETE' && request.url().includes(MESSAGES_PATH)) deletes.push(request.url());
  });

  await clear.click();
  const dialog = page.getByTestId('chat-delete-confirm-dialog');
  await expect(dialog, 'the clear must be confirmed, never performed on the first click').toBeVisible({ timeout: 10_000 });
  await expect(dialog).toContainText('Clear chat');
  await expect(dialog).toContainText("The deleted messages can't be restored.");
  await dialog.getByRole('button', { name: 'Cancel' }).click();

  await expect(dialog).toHaveCount(0);
  await expect(page.getByTestId('user-message')).toHaveCount(2);
  // Read after a round trip, so a request the Cancel issued has left the
  // browser by the time it is counted.
  await page.evaluate(() => undefined);
  expect(deletes, 'Cancel must not reach the delete-all route').toEqual([]);

  // ── the confirm path ──────────────────────────────────────────────────────
  // Anything set on this page object dies with a reload. Read back below, so
  // the "the transcript emptied" assertion cannot be satisfied by a reload —
  // which would pass against the very defect this journey is about.
  await page.evaluate(() => {
    (window as unknown as { __eliteaClear?: string }).__eliteaClear = 'alive';
  });

  await clear.click();
  await expect(dialog).toBeVisible({ timeout: 10_000 });

  const cleared = page.waitForResponse(
    (response) => response.url().includes(MESSAGES_PATH) && response.request().method() === 'DELETE',
    { timeout: 30_000 },
  );
  await dialog.getByRole('button', { name: 'Delete' }).click();
  const clearResponse = await cleared;
  expect(clearResponse.status(), 'the confirmed clear must be accepted by the server').toBe(204);

  await expect(page.getByTestId('user-message')).toHaveCount(0, { timeout: 20_000 });
  await expect(page.getByTestId('error-trace')).toHaveCount(0);
  await expect(clear, 'an emptied transcript makes the control unofferable again').toBeDisabled({ timeout: 20_000 });
  expect(
    await page.evaluate(() => (window as unknown as { __eliteaClear?: string }).__eliteaClear),
    'the transcript must empty without a reload — a reload would pass this against the bug it exists for',
  ).toBe('alive');

  // ── and it survives a reload ──────────────────────────────────────────────
  // The conversation itself must NOT have been deleted along with its
  // messages: the delete-all route and the delete-conversation route differ by
  // one path segment, and a clear wired to the wrong one empties the screen
  // just as convincingly.
  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 30_000 });
  await expect(page.getByText('Something went wrong.')).toHaveCount(0);
  await expect(page.getByTestId('user-message')).toHaveCount(0);
  expect(await storedGroupCount(page, conversationId), 'the stored transcript is empty').toBe(0);

  await checkA11y(page);
  await deleteConversation(page.request, conversationId);
});
