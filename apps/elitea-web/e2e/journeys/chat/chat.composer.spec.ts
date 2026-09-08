/**
 * The composer takes keystrokes without throwing (regression for PR #597).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS IS AN E2E TEST AND NOT A UNIT TEST
 * ─────────────────────────────────────────────────────────────────────────────
 * `ChatBox` used to attach its own `ChatBoxHandle` with
 * `useImperativeHandle(chatInputRef, ...)` — the SAME ref it passes to
 * `<NewChatInput ref={chatInputRef}>`. The child's handle is attached first
 * and the parent's overwrote it on commit, so `chatInputRef.current` held
 * `{ onClear, mentionUser, stopAll }` and nothing else. Every keystroke then
 * threw out of the "/" and "~" mention hooks:
 *
 *   Uncaught TypeError: n.current?.getCursorPosition is not a function
 *       at onInputChange (chat-*.js)
 *
 * Chat was unusable in production while the whole unit suite was green: 2475
 * tests over `widgets/chat-box` + `features/chat-input` pass with the bug in
 * place, because the two halves are only ever mounted together by `ChatBox`
 * itself, and no unit test mounts it. This is the defect class where the
 * wiring between two individually-correct components is the bug, and the only
 * harness that can see it is one that renders the real composition root and
 * presses a key.
 *
 * The existing chat journeys press keys too — and passed with the bug — for
 * one reason: `locator.fill()` sets the value in a single DOM event, and
 * neither they nor Playwright fail a test on an uncaught page exception unless
 * something is listening for it. This spec listens.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT IT ASSERTS
 * ─────────────────────────────────────────────────────────────────────────────
 *  1. No uncaught exception reaches the page while typing, ONE CHARACTER AT A
 *     TIME (`pressSequentially`, not `fill`) — the clobbered handle throws on
 *     the first keystroke, since `useSlashMention.onInputChange` calls
 *     `chatInput.current.getCursorPosition()` for any non-empty value, with no
 *     participant/toolkit/skill data needed to reach it.
 *  2. The trigger characters "@", "/" and "~" are typed as part of the text,
 *     so all three mention state machines run their detection, not just the
 *     plain-text path.
 *  3. The composer still holds exactly what was typed afterwards, and the send
 *     control appeared — i.e. the input is functional, not merely quiet.
 *
 * It deliberately does NOT assert on any mention dropdown: this stack seeds no
 * co-participants, toolkits or skills for the member persona, so a visible "/"
 * or "~" list is not a thing this stack can produce. Asserting one would be a
 * test of the seed, and the guarded `if (visible)` shape that would let it
 * "pass" anyway is exactly what this file's neighbours had to have removed.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE TWO KEYBOARD RULES OF THE COMPOSER (ported from the legacy suite)
 * ─────────────────────────────────────────────────────────────────────────────
 * Added with the legacy public suite's port
 * (`qa/elitea-testing-public/automation/tests/ui/chat/test_chat_interface.py`):
 *
 *  - `TestSendingMessages::test_shift_enter_adds_new_line` (TC-CHAT-006)
 *  - `TestSendingMessages::test_cannot_send_empty_message` (TC-CHAT-007)
 *
 * Both are keyboard SEMANTICS, which is why they belong beside the keystroke
 * smoke above rather than in a new file: one key combination must NOT send and
 * must grow the value, the other must send, and an empty value must do
 * neither. `useUserInputKeyHandling` binds `onShiftEnterPressed` to the same
 * `insertTextAtCursor('\n')` as Ctrl+Enter and `onEnterDown` to `sendQuestion`,
 * so a single wrong binding swaps the two — a swap that no unit test over
 * `useCtrlEnterKeyEventsHandler` can see, because it is decided by which
 * callback `UserInput` passes, not by the dispatch it tests.
 *
 * Each of the two asserts the DISCRIMINATOR, not just its own half: the
 * newline test goes on to send the same three lines with a plain Enter and
 * reads the conversation the server created for them, and the empty test
 * counts the create requests over its whole run so that "nothing was sent"
 * is measured against a send that really did happen afterwards. A test that
 * only pressed Shift+Enter and looked at a textarea would pass against a
 * composer that never sends anything at all — which is exactly the state
 * this file's first test exists because of.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, deleteConversation } from '../../fixtures/api';

/**
 * Contains all three mention triggers. "@" and "~" are preceded by a space and
 * "/" follows a word, so `detectMention`'s "start-of-text or whitespace"
 * precondition holds for the two that need it — the point is to enter the
 * detection branches, not just the early return.
 */
const TYPED = 'hi @a b/c ~d';

test('the composer takes keystrokes without throwing (#597)', async ({ page }) => {
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 15_000 });

  // Registered only now, so unrelated errors from page load (a peripheral 401
  // re-auth, a slow socket) can never be attributed to a keystroke. Everything
  // collected below happened while typing.
  const pageErrors: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.stack ?? String(error)));

  await input.click();
  await input.pressSequentially(TYPED, { delay: 20 });

  // The input is quiet AND working: it kept every character, and the send
  // control — which `ChatBox` renders only in response to input — appeared.
  await expect(input).toHaveValue(TYPED);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 5_000 });

  // Read LAST, and only after a round-trip to the page. `pageerror` is
  // delivered asynchronously over CDP, so a throw from the FINAL keystroke —
  // or from something that keystroke scheduled — can still be in flight when
  // `pressSequentially` resolves; read on the next line it would be missed and
  // this test would go green on a broken composer. Protocol events arrive in
  // order on one connection, so awaiting a round-trip issued after the last
  // keypress guarantees every error the typing already raised has landed.
  //
  // Note it must be a round-trip and not `expect.poll`: polling for an EMPTY
  // array passes on its first read, so it would not wait at all.
  await page.evaluate(() => undefined);
  expect(pageErrors, 'typing into the composer must not throw').toEqual([]);
});

/** Every conversation this file's two send tests create carries this suffix. */
const SUFFIX = '-comp';

/** The route a send creates the conversation on — counted, and read back, below. */
const CONVERSATIONS_PATH = `/elitea_core/conversations/prompt_lib/${DEFAULT_PROJECT_ID}`;
const CONVERSATION_PATH = `/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;

/**
 * One create request, as the browser really issued it.
 *
 * Counting REQUESTS rather than responses is deliberate: a send that reached
 * the network and was refused still means the composer sent something, and the
 * empty-message rule is about not reaching the network at all.
 */
function countCreateRequests(page: import('@playwright/test').Page): { readonly total: () => number } {
  let seen = 0;
  page.on('request', (request) => {
    if (request.method() === 'POST' && request.url().includes(CONVERSATIONS_PATH)) seen += 1;
  });
  return { total: () => seen };
}

// ─────────────────────────────────────────────────────────────────────────────
// legacy: test_chat_interface.py::TestSendingMessages::test_shift_enter_adds_new_line
// ─────────────────────────────────────────────────────────────────────────────
test('Shift+Enter grows the message by a line, and plain Enter sends all of it', async ({ page }) => {
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 15_000 });
  const creates = countCreateRequests(page);

  // The first line carries the prefix so the conversation the send creates is
  // named `autotest_…` and is sweepable if this test fails before its delete.
  const first = `${AUTOTEST_PREFIX}shift${SUFFIX}-${Date.now()}`;
  const typed = `${first}\nline two\nline three`;

  await input.click();
  await input.pressSequentially(first);
  await page.keyboard.press('Shift+Enter');
  await input.pressSequentially('line two');
  await page.keyboard.press('Shift+Enter');
  await input.pressSequentially('line three');

  // The two newlines are IN the value — `insertTextAtCursor('\n')`, not a
  // browser default: the handler calls `preventDefault()` first, so a textarea
  // that grew a line on its own would mean the binding never ran.
  await expect(input).toHaveValue(typed);
  // …and neither of them sent. Read after a round-trip so a request the last
  // keypress scheduled has been issued by the time this is counted.
  await page.evaluate(() => undefined);
  expect(creates.total(), 'Shift+Enter must not create a conversation').toBe(0);

  // Plain Enter, same composer, same content: this one must send.
  const created = page.waitForResponse(
    (response) => response.url().includes(CONVERSATIONS_PATH) && response.request().method() === 'POST',
    { timeout: 20_000 },
  );
  await page.keyboard.press('Enter');
  const response = await created;
  expect(response.status()).toBe(201);
  const body = (await response.json()) as { id?: string; name?: string };
  expect(body.id).toMatch(/^\d+$/);

  // The multi-line question is one message, not three: the composer cleared
  // once and the transcript holds a single user bubble carrying every line.
  await expect(input).toHaveValue('');
  const userMessage = page.getByTestId('user-message');
  await expect(userMessage).toHaveCount(1);
  await expect(userMessage.first()).toContainText(first);
  await expect(userMessage.first()).toContainText('line three');
  expect(creates.total(), 'exactly one send, and it was the plain-Enter one').toBe(1);

  await deleteConversation(page.request, body.id as string);
});

// ─────────────────────────────────────────────────────────────────────────────
// legacy: test_chat_interface.py::TestSendingMessages::test_cannot_send_empty_message
// ─────────────────────────────────────────────────────────────────────────────
test('an empty or blank composer sends nothing, and the send control is not even rendered', async ({ page }) => {
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 15_000 });
  const creates = countCreateRequests(page);

  // While the composer is empty there is no send control at all — `ChatBox`
  // renders it in response to input, so a page that always painted one would
  // fail here before any keystroke.
  await expect(page.getByTestId('chat-send-button')).toHaveCount(0);

  await input.click();
  await page.keyboard.press('Enter');
  // Whitespace is empty too: `createSendQuestion` refuses on `question.trim()`.
  await input.fill('   ');
  await page.keyboard.press('Enter');

  // `count()` for the absence — it reports what is mounted now instead of
  // waiting for an element to attach, which is what makes it safe to assert a
  // transcript that must stay empty.
  await expect(page.getByTestId('user-message')).toHaveCount(0);

  // The proof that the two refusals are refusals and not a dead keyboard: the
  // SAME composer, given real text, sends on the same key.
  const text = `${AUTOTEST_PREFIX}empty${SUFFIX}-${Date.now()}`;
  const created = page.waitForResponse(
    (response) => response.url().includes(CONVERSATIONS_PATH) && response.request().method() === 'POST',
    { timeout: 20_000 },
  );
  await input.fill(text);
  await page.keyboard.press('Enter');
  const response = await created;
  expect(response.status()).toBe(201);
  const body = (await response.json()) as { id?: string; name?: string };
  expect(body.name, 'the conversation is named after the question that opened it').toBe(text);

  await expect(page.getByTestId('user-message')).toHaveCount(1);
  expect(
    creates.total(),
    'the two blank Enters must contribute no create request — only the real one counted',
  ).toBe(1);

  // The row really is in the database under that name, so "one create" is not
  // just one request that could have been refused.
  const read = await page.request.get(`${API_BASE}${CONVERSATION_PATH}/${body.id}`);
  expect(read.status()).toBe(200);
  expect(((await read.json()) as { name?: string }).name).toBe(text);

  await deleteConversation(page.request, body.id as string);
});
