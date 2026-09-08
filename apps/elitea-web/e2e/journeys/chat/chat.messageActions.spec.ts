/**
 * What a user can DO to a message that is already on screen, and what the app
 * says when a turn could not be delivered.
 *
 * Ported by use case from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/chat/test_chat_interface.py`):
 *
 *  - `TestMessageActions::test_copy_message_to_clipboard` (TC-CHAT-008)
 *  - `TestSearchAndErrorHandling::test_handle_message_send_failure` (TC-CHAT-024)
 *
 * The third legacy message action, `TestMessageActions::test_delete_message`
 * (TC-CHAT-009), is NOT here and cannot be: `ChatMessageList` offers Delete on
 * the LAST message only, and confirming it issues
 * `DELETE /elitea_core/messages/prompt_lib/{p}/{c}/{groupUID}`, which
 * `ConversationsRepo.DeleteMessage` answers 404 for a uuid with no
 * `chat_message_group` row. This stack persists no message group at all (see
 * `chat.management.spec.ts`'s module header, note 1), so the only bubble on
 * screen here is an optimistic client-side one and the delete could only ever
 * fail. It is ported instead as `e2e/streaming/chat.message-delete.spec.ts`,
 * on the `chat-stream` project, where a real turn writes the rows the delete
 * is about.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * HOW THE COPY IS OBSERVED, AND WHY NOT THROUGH THE REAL CLIPBOARD
 * ─────────────────────────────────────────────────────────────────────────────
 * The system clipboard is not readable in both engines this project runs:
 * Playwright's `clipboard-read`/`clipboard-write` permissions are Chromium-only,
 * and a WebKit run cannot grant them, so a test that read the clipboard back
 * would either be Chromium-gated or flaky. What is asserted instead is the
 * boundary the product controls: the exact string the app hands to
 * `navigator.clipboard.writeText`. A recorder is installed over that one method
 * in an init script, BEFORE the app loads.
 *
 * That is a stub of a PLATFORM api, never of the app: `useChatBoxHandlers`'s
 * `copyToClipboard` still runs, `extractCopyableContent` still selects what to
 * copy from the real message object, and `ChatMessageList` still decides which
 * bubble the button belongs to. The two defects this discriminates are the ones
 * that actually happen here — a Copy control wired to no handler (nothing is
 * recorded) and a handler that copies the wrong thing, e.g. the whole message
 * object or a neighbouring bubble (something else is recorded). What it cannot
 * see is the operating system's own paste buffer, which no assertion in this
 * suite depends on.
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, deleteConversation } from '../../fixtures/api';

/** Every conversation this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-msg';

const CONVERSATIONS_PATH = `/elitea_core/conversations/prompt_lib/${DEFAULT_PROJECT_ID}`;

/** `ChatBox` derives the conversation name from the question, truncated to 50 chars. */
const MAX_NAME = 50;

/** Exact accessible name of the per-message copy control (`UserMessageActions.tsx`). */
const COPY_NAME = 'Copy to clipboard';

interface ClipboardWindow {
  __eliteaClipboardWrites?: string[];
}

/**
 * Records every `navigator.clipboard.writeText` argument into the page.
 *
 * `Navigator.prototype.clipboard` is a getter, so the replacement is installed
 * there rather than by assigning to `navigator.clipboard` (which silently does
 * nothing on an accessor property, and would have made every assertion below
 * read an array that stays empty for a reason unrelated to the product).
 */
async function recordClipboardWrites(page: import('@playwright/test').Page): Promise<void> {
  await page.addInitScript(() => {
    const target = window as unknown as ClipboardWindow;
    target.__eliteaClipboardWrites = [];
    Object.defineProperty(Navigator.prototype, 'clipboard', {
      configurable: true,
      get: () => ({
        writeText: (text: string) => {
          (window as unknown as ClipboardWindow).__eliteaClipboardWrites?.push(text);
          return Promise.resolve();
        },
      }),
    });
  });
}

/** What the page has copied so far, oldest first. */
async function clipboardWrites(page: import('@playwright/test').Page): Promise<readonly string[]> {
  return page.evaluate(() => (window as unknown as ClipboardWindow).__eliteaClipboardWrites ?? []);
}

/**
 * Types `text` into the composer, sends it, and returns the conversation the
 * backend created for it.
 *
 * Verified against the real POST rather than a spinner, exactly as
 * `chat.conversation.spec.ts`'s own send helper does: a UI that renders the
 * bubble optimistically and never reaches the server fails here.
 */
async function sendFirstMessage(
  page: import('@playwright/test').Page,
  text: string,
): Promise<string> {
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 15_000 });

  const created = page.waitForResponse(
    (response) => response.url().includes(CONVERSATIONS_PATH) && response.request().method() === 'POST',
    { timeout: 20_000 },
  );
  await input.fill(text);
  await page.getByTestId('chat-send-button').click();
  const response = await created;
  expect(response.status(), 'the send must create the conversation').toBe(201);

  const body = (await response.json()) as { id?: string };
  expect(body.id).toMatch(/^\d+$/);
  return body.id as string;
}

// ─────────────────────────────────────────────────────────────────────────────
// legacy: test_chat_interface.py::TestMessageActions::test_copy_message_to_clipboard
//
// The legacy test copied the ASSISTANT's answer, which needs a turn. The use
// case — "a message's copy button puts THAT message's text on the clipboard" —
// is the same control on the same row (`UserMessageActions` /
// `ApplicationAnswerActions` both receive `handleCopy` from one
// `onCopyToClipboard` in `ChatMessageList`), so it is asserted here on the
// message this stack can really produce.
// ─────────────────────────────────────────────────────────────────────────────
test('the copy control on a message hands that message’s own text to the clipboard', async ({ page }) => {
  await recordClipboardWrites(page);
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  const text = `${AUTOTEST_PREFIX}copy${SUFFIX}-${Date.now()}`;
  expect(text.length, 'the message must fit the 50-char name truncation').toBeLessThanOrEqual(MAX_NAME);
  const conversationId = await sendFirstMessage(page, text);

  const message = page.getByTestId('user-message').first();
  await expect(message).toContainText(text);

  // Nothing has been copied yet — the recorder is empty before the click, so
  // what it holds afterwards was put there BY the click.
  expect(await clipboardWrites(page)).toEqual([]);

  await message.hover();
  const copy = message.getByRole('button', { name: COPY_NAME });
  await expect(copy).toBeVisible({ timeout: 5_000 });
  await copy.click();

  // Polled: `copyToClipboard` is async, so the write lands a microtask after
  // the click resolves.
  await expect
    .poll(async () => (await clipboardWrites(page)).length, {
      timeout: 10_000,
      message: 'the copy control must reach the clipboard — an unwired button records nothing',
    })
    .toBe(1);

  // The EXACT text of this message, not a summary, not the message object, and
  // not a neighbouring bubble's content.
  expect(await clipboardWrites(page)).toEqual([text]);

  await checkA11y(page);
  await deleteConversation(page.request, conversationId);
});

// ─────────────────────────────────────────────────────────────────────────────
// legacy: test_chat_interface.py::TestSearchAndErrorHandling::test_handle_message_send_failure
//
// The legacy test sent 100 000 characters and accepted any of three outcomes
// (an error, a silent drop, or a successful send) — it could only state "the
// app did not crash". This stack can state the whole rule, because it is the
// stack in which every turn takes the undeliverable path: `VITE_SOCKET_SERVER`
// is empty (`deploy/docker-compose.e2e-standalone.yml`), so
// `createNoopSocketClient` emits nothing and there is no runtime plane to start
// a streamed execution, which is exactly what a dropped transport looks like.
//
// `historyAfterFailedTurn` is what must then happen, and all of it is asserted:
// the question STAYS on screen (the composer has already been cleared, so the
// bubble is the only copy of what the person typed), and a failure anchored to
// that question is rendered beside it. The regression this guards is the one
// that shipped: the failed turn used to drop the question unconditionally, so a
// send that could not be delivered emptied the transcript and told nobody.
// ─────────────────────────────────────────────────────────────────────────────
test('a turn no transport accepted keeps the question and says so, even at 100k characters', async ({ page }) => {
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  const marker = `${AUTOTEST_PREFIX}oversized${SUFFIX}-${Date.now()}`;
  // The prefix leads so the conversation the server stores is named
  // `autotest_…` (the name is the question truncated to 50 chars) and the sweep
  // can find it; the 100 000 characters after it are the legacy stimulus.
  const oversized = `${marker} ${'A'.repeat(100_000)}`;

  // Registered before the send, so an exception raised while rendering a
  // 100 000-character bubble is attributed to this send and not to page load.
  const pageErrors: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.stack ?? String(error)));

  const conversationId = await sendFirstMessage(page, oversized);

  // The composer cleared, and the question survived that clearing.
  await expect(page.getByTestId('chat-message-input')).toHaveValue('');
  const question = page.getByTestId('user-message');
  await expect(question).toHaveCount(1);
  await expect(question.first()).toContainText(marker);

  // The failure is reported ON the transcript, next to the question it belongs
  // to — `buildFailedTurnMessage` gives the error bubble `${questionId}-error`,
  // and `ApplicationAnswer` renders its `exception` through `ErrorTrace`.
  const failure = page.getByTestId('error-trace');
  await expect(failure).toBeVisible({ timeout: 20_000 });
  await expect(failure).toContainText('The message was not sent');

  // Graceful means graceful: nothing threw while any of that happened.
  await page.evaluate(() => undefined);
  expect(pageErrors, 'an undeliverable turn must not raise an uncaught exception').toEqual([]);

  await checkA11y(page);
  await deleteConversation(page.request, conversationId);
});
