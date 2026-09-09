/**
 * Journey 8: Create conversation → send message → stream tokens → stop (JRNY-008)
 * Journey 9: Regenerate a response (JRNY-009)
 * Journey 11: Rename conversation server-side → live update (JRNY-011)
 *
 * Spec §8.5 acceptance (from parity/manifest/chat.json JRNY-008/009/011).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THESE THREE JOURNEYS CAN AND CANNOT ASSERT IN THE E2E STACK
 * ─────────────────────────────────────────────────────────────────────────────
 * The previous revision of this file passed in 1.6 s while asserting nothing:
 * every part that could not hold was guarded by `.or(...)` fallbacks,
 * `.isVisible().catch(() => false)` probes and `if (...)` blocks whose else
 * branch was "silently succeed". Those are gone. What replaced them, and what
 * genuinely cannot be built into an assertion here, is recorded below —
 * measured against the running stack, not inferred from source.
 *
 * 1. TOKEN STREAMING AND THE STOP CONTROL ARE UNREACHABLE IN THIS STACK.
 *    `deploy/docker-compose.e2e-standalone.yml` sets `VITE_SOCKET_SERVER: ""`.
 *    With an empty socket URL the app builds `createNoopSocketClient()`
 *    (`src/shared/api/socket/client.ts`) — `emit`/`on` are no-ops and the
 *    connection state is permanently 'disconnected'. Measured: sending a
 *    message opens ZERO websockets and issues exactly one request
 *    (`POST …/conversations/prompt_lib/1` → 201). No assistant message ever
 *    arrives, therefore no stop button and no assistant-side regenerate
 *    button can ever render. Asserting on them would either hang or —
 *    as before — be skipped by a soft `if`. Both are documented here and
 *    left unasserted deliberately. Making them testable needs a socket
 *    server (or a deterministic stub emitter) in the E2E compose file; that
 *    is a stack change, not a spec change.
 *
 * 2. THERE IS NO CONVERSATION LIST IN THE APP.
 *    `src/features/chat-conversation-list/**` is imported by nothing outside
 *    itself (verified: every hit for the slice name outside the slice is a
 *    doc comment or `shared/api/endpoints.manifest.json`). The shell sidebar
 *    renders nav entries only. So JRNY-011's literal acceptance — "the list
 *    row updates live" — has no UI surface to observe at all. J11 below
 *    asserts the half that does exist end to end (the rename persists and
 *    the renamed conversation opens at its own deep link) instead of
 *    pretending to observe a list.
 *
 * 3. `/chat` NEVER NAVIGATES TO `/chat/:newId`.
 *    `src/pages/chat/index.tsx` documents this as a known gap (`ChatBoxProps`
 *    has no "conversation was created" callback). Measured: after a send the
 *    URL is still `/app/chat`. The old test read a conversation id out of the
 *    URL and wrapped its whole body in `if (conversationId)` — which was
 *    therefore ALWAYS false, so J11 asserted literally nothing. J11 now takes
 *    the id from the API response instead.
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

/** Every entity this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-conv';

/** `ChatBox` derives the conversation name from the question, truncated to 50 chars. */
const MAX_NAME = 50;

const CONVERSATIONS_PATH = `/elitea_core/conversations/prompt_lib/${DEFAULT_PROJECT_ID}`;
const CONVERSATION_PATH = `/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;

function uniqueMessage(tag: string): string {
  const text = `${AUTOTEST_PREFIX}${tag}-${Date.now()}${SUFFIX}`;
  expect(text.length, 'test message must fit the 50-char name truncation').toBeLessThanOrEqual(MAX_NAME);
  return text;
}

/**
 * Types `text` into the chat composer and sends it, returning the conversation
 * the backend created for it.
 *
 * The send is verified against the real POST — not against a spinner — so a
 * UI that renders the message optimistically and never reaches the server
 * fails here rather than passing.
 */
async function sendFirstMessage(
  page: import('@playwright/test').Page,
  text: string,
): Promise<{ readonly id: string; readonly uuid: string; readonly name: string }> {
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 15_000 });

  // The send control is not rendered at all while the composer is empty; it
  // appears in response to input. A stub that always paints a button fails.
  await expect(page.getByTestId('chat-send-button')).toHaveCount(0);
  await input.fill(text);
  const sendButton = page.getByTestId('chat-send-button');
  await expect(sendButton).toBeEnabled({ timeout: 5_000 });

  const created = page.waitForResponse(
    (r) => r.url().includes(CONVERSATIONS_PATH) && r.request().method() === 'POST',
    { timeout: 20_000 },
  );
  await sendButton.click();
  const response = await created;
  expect(response.status(), 'POST conversations must create the conversation').toBe(201);

  const body = (await response.json()) as { id?: string; uuid?: string; name?: string; message_count?: number };
  // Backend-derived values a stub page cannot fabricate: a serial id, a real
  // uuid, and the name the SERVER stored for the question we typed.
  expect(body.id).toMatch(/^\d+$/);
  expect(body.uuid).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
  expect(body.name, 'the conversation is named after the question that opened it').toBe(text);
  expect(body.message_count).toBe(0);

  return { id: body.id as string, uuid: body.uuid as string, name: body.name as string };
}

// ─────────────────────────────────────────────────────────────────────────────
// Journey 8: Create conversation, send message
// (streaming + stop: see note 1 in the module header — unreachable in-stack)
// ─────────────────────────────────────────────────────────────────────────────
test('J8: sending the first message creates and persists a real conversation', async ({ page }) => {
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
  await checkA11y(page);

  const text = uniqueMessage('j8');
  const conversation = await sendFirstMessage(page, text);

  // The composer clears and the question is rendered as a user message inside
  // the message list — both only exist once a send actually happened.
  await expect(page.getByTestId('chat-message-input')).toHaveValue('');
  const list = page.getByTestId('chat-message-list');
  await expect(list).toBeVisible({ timeout: 10_000 });
  await expect(list.getByTestId('user-message')).toHaveCount(1);
  await expect(list.getByTestId('user-message').first()).toContainText(text);

  // Independent read-back through the app's own authenticated session: the row
  // is really in the database, under the name derived from the question.
  const fetched = await page.request.get(`${API_BASE}${CONVERSATION_PATH}/${conversation.id}`);
  expect(fetched.status()).toBe(200);
  const detail = (await fetched.json()) as { name?: string; id?: string };
  expect(detail.id).toBe(conversation.id);
  expect(detail.name).toBe(text);

  await checkA11y(page);
});

// ─────────────────────────────────────────────────────────────────────────────
// The paired exchange, and where it is proved
//
// The legacy API suite pins the shape of a completed turn: one POST leaves the
// conversation holding exactly TWO message groups, the question and the answer,
// tied together — the reply names the group it answers, and the question names
// the participant it was sent to. That pairing is written by the runtime plane,
// and this stack composes none (module header, note 1), so it is proved in the
// `chat-stream` project instead, where a turn really runs.
//
// What is proved HERE is the property that makes the absence honest, and it is
// not visible anywhere else: after a send that this deployment cannot run, the
// SERVER's transcript is still empty. J8 above asserts that the question is
// rendered in the message list — a purely client-side bubble — so without this
// case the suite would state "the message is there" about a conversation that
// holds nothing, and a regression that stored the question without ever
// answering it would look exactly the same.
//
// Both read paths are asserted because they are different projections and have
// disagreed: the transcript route aggregates text items per group, while the
// conversation read embeds `message_groups` and only when `messages_limit` is
// supplied. A route that answered a well-formed empty page for a failing query
// is defect #599's shape, and reading only one of the two would not notice it.
// ─────────────────────────────────────────────────────────────────────────────
test('J8: a send this deployment cannot run leaves the server transcript empty', async ({ page }) => {
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  const text = uniqueMessage('j8b');
  const conversation = await sendFirstMessage(page, text);

  // The client-side bubble, which is what makes the reads below necessary.
  await expect(page.getByTestId('user-message').first()).toContainText(text);

  const transcript = await page.request.get(
    `${API_BASE}/elitea_core/messages/prompt_lib/${DEFAULT_PROJECT_ID}/${conversation.id}` +
      '?sort_order=asc&limit=100',
  );
  expect(transcript.status()).toBe(200);
  const items = ((await transcript.json()) as { items?: readonly unknown[]; total?: number }).items ?? [];
  expect(
    items,
    'the question was stored with no runtime plane to answer it — the user would wait forever',
  ).toEqual([]);

  const embedded = await page.request.get(
    `${API_BASE}${CONVERSATION_PATH}/${conversation.id}?messages_limit=50&sort_order=asc`,
  );
  expect(embedded.status()).toBe(200);
  const detail = (await embedded.json()) as { message_groups?: readonly unknown[]; message_count?: number };
  expect(detail.message_groups ?? [], 'the two read paths disagree about this conversation').toEqual([]);
  expect(detail.message_count, 'the conversation counts a message it does not hold').toBe(0);
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 9: Regenerate a response
//
// The assistant-side "regenerate" button hangs off an assistant message, which
// cannot exist without a socket server (module header, note 1). What IS built
// and reachable is the regenerate ENTRY POINT on the user's own message —
// "Edit the message and regenerate answer" — including its edit-mode form and
// its dirty-state gate. That is what this test drives; the re-answer itself is
// out of reach here and is deliberately not faked.
// ─────────────────────────────────────────────────────────────────────────────
test('J9: the edit-and-regenerate control opens a dirty-gated editor on the user message', async ({ page }) => {
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  const text = uniqueMessage('j9');
  await sendFirstMessage(page, text);

  const userMessage = page.getByTestId('user-message').first();
  await expect(userMessage).toContainText(text);
  await userMessage.hover();

  const regenerate = userMessage.getByRole('button', { name: /edit the message and regenerate answer/i });
  await expect(regenerate).toBeVisible({ timeout: 5_000 });
  await regenerate.click();

  // Edit mode replaces the rendered bubble with a real, prefilled form control.
  const editor = userMessage.getByRole('textbox');
  await expect(editor).toHaveValue(text);

  const save = userMessage.getByRole('button', { name: 'Save and apply' });
  const cancel = userMessage.getByRole('button', { name: 'Cancel' });
  // Unmodified content must not be submittable — the dirty gate is behaviour a
  // stub cannot reproduce, since it depends on comparing the live field value
  // with the message's own content.
  await expect(save).toBeDisabled();
  await editor.fill(`${text} edited`);
  await expect(save).toBeEnabled();

  // Cancelling restores the ORIGINAL content, not the edited draft.
  await cancel.click();
  await expect(userMessage.getByRole('textbox')).toHaveCount(0);
  await expect(userMessage).toContainText(text);
  await expect(userMessage).not.toContainText('edited');

  await checkA11y(page);
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 11: Server-side rename
//
// "Live update in the conversation list" has no UI surface (module header,
// note 2). The reachable half of the journey is asserted instead: a rename
// performed outside the tab persists, and the renamed conversation opens at
// its own deep link `/app/chat/:conversationId`.
//
// KNOWN FAILURE — this test is currently RED against a real defect, and is
// left red on purpose rather than weakened:
//   `src/pages/chat/useChatPageData.ts:77` unwraps the message-list response as
//   `'rows' in response ? response.rows : response`, but
//   `GET /elitea_core/messages/prompt_lib/{p}/{c}` returns
//   `{items, total, page, page_size, total_pages}`. Neither branch matches, so
//   the whole envelope object is handed to `ChatBox` as `message_groups`, and
//   `convertMessagesToChatHistory.ts:216`'s `[...(messageGroups ?? [])]` throws
//   "(e ?? []) is not iterable". The route's error boundary swallows the chat
//   page. Verified: every deep link to `/app/chat/:id` renders "Something went
//   wrong." while both underlying API calls return 200.
// ─────────────────────────────────────────────────────────────────────────────
test('J11: a server-side rename persists and the conversation opens at its deep link', async ({ page }) => {
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  const originalName = uniqueMessage('j11');
  const conversation = await sendFirstMessage(page, originalName);

  // Rename from outside the tab, exactly as a server-side auto-naming pass would.
  const renamed = `${AUTOTEST_PREFIX}renamed-${Date.now()}${SUFFIX}`;
  const put = await page.request.put(`${API_BASE}${CONVERSATION_PATH}/${conversation.id}`, {
    data: { name: renamed },
  });
  expect(put.status()).toBe(200);
  expect(((await put.json()) as { name?: string }).name).toBe(renamed);

  // The rename is durable, not just echoed back by the write.
  const afterRename = await page.request.get(`${API_BASE}${CONVERSATION_PATH}/${conversation.id}`);
  expect(afterRename.status()).toBe(200);
  const detail = (await afterRename.json()) as { name?: string; uuid?: string };
  expect(detail.name).toBe(renamed);
  expect(detail.name).not.toBe(originalName);

  // The renamed conversation must open at its own URL. See KNOWN FAILURE above.
  // Settled state only: the composer does render for a frame before the
  // conversation query resolves and the boundary takes over, so asserting
  // without waiting for the route's own requests to finish would sometimes
  // catch the pre-crash frame and call it a pass.
  await page.goto(`${BASE_URL}/app/chat/${conversation.id}`);
  await page.waitForLoadState('networkidle');
  await expect(page.getByText('Something went wrong.')).toHaveCount(0);
  await expect(page.getByTestId('chat-message-input')).toBeEditable();
  expect(page.url()).toContain(`/app/chat/${conversation.id}`);

  await checkA11y(page);
});
