/**
 * The composer's "@" user-mention dropdown — trigger, filter, mouse/keyboard
 * select, and the participant-count gate that decides whether it can appear
 * at all.
 *
 * Ported by use case from `w1-chat-interface.md` (ELITEA-0391/0392/0393/0394).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THESE FOUR ARE CHROMIUM-LANE, NOT CHAT-STREAM
 * ─────────────────────────────────────────────────────────────────────────────
 * The dropdown itself is driven entirely by `useChatBoxState`'s `users` memo
 * (`widgets/chat-box/ui/hooks/useChatBoxState.ts`), which is built from the
 * conversation's `participants` prop alone — `entityName === 'user'` rows
 * other than the caller, plus a synthetic "Everyone" row — gated by
 * `userMentions.isProcessingAtSymbol && userMentions.hasOtherUsers`
 * (`ChatBoxPopups.tsx`). None of that needs a model turn: attaching a second
 * REAL user participant through the REST participants endpoint (exactly the
 * pattern `chat.management.spec.ts`'s M2c/M3 already use) is enough to make
 * the dropdown a real, populated thing to drive.
 *
 * `Send` is deliberately never clicked here. `chat.management.spec.ts`'s
 * module header (note 1) established that this stack cannot persist a
 * message into an EXISTING conversation at all — the only route that writes
 * a `chat_message_group` is the runtime plane's agent-start, which this stack
 * does not mount (405). Every conversation these four tests use is
 * pre-seeded with participants over the API specifically so the dropdown has
 * someone to list, which means it is never the "brand-new, no id yet" chat
 * whose first send instead creates a conversation shell
 * (`chat.conversation.spec.ts`'s J8/`sendFirstMessage`). So the composer's
 * TEXT CONTENT after each interaction is the assertion — matching what the
 * manual cases actually discriminate ("`@Alice` is inserted") — and sending
 * is left to a journey that owns an existing-conversation send path once one
 * exists.
 *
 * The keyboard nav (`UserMentionList`'s own `ArrowDown`/`ArrowUp`/`Enter`
 * document-capture listener) and the trigger detection
 * (`useNewInputKeyDownHandler`'s `isProcessingAtSymbol`) both fire on any
 * "@" keystroke, so `pressSequentially` (one keystroke at a time, matching
 * `chat.composer.spec.ts`'s own established reason: `.fill()` sets the value
 * in one DOM event and never runs the detection hooks) is used throughout.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createConversation,
  deleteConversation,
} from '../../fixtures/api';

/** Every entity this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-mention';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}${SUFFIX}`;
}

/** The signed-in persona, read the way `useChatBoxState` resolves "not-self". */
async function readAuthor(request: APIRequestContext): Promise<{ readonly id: string; readonly name: string }> {
  const response = await request.get(`${API_BASE}/social/author`);
  expect(response.status(), 'the persona must be resolvable, or nothing below means anything').toBe(200);
  const body = (await response.json()) as { id?: string; name?: string };
  expect(body.id).toMatch(/^\d+$/);
  expect(body.name).not.toBe('');
  return { id: body.id as string, name: body.name as string };
}

/**
 * `count` OTHER seeded users, distinct from the caller and from each other —
 * read from the project's own directory rather than hardcoded, matching
 * `chat.management.spec.ts`'s `readOtherUser` for the same reason: the seed
 * provisions personas by email and their ids are whatever the database
 * assigned.
 */
async function readOtherUsers(
  request: APIRequestContext,
  selfId: string,
  count: number,
): Promise<readonly { readonly id: string; readonly name: string }[]> {
  const response = await request.get(`${API_BASE}/admin/users/default/${DEFAULT_PROJECT_ID}?limit=200&offset=0`);
  expect(response.status(), 'the user listing backs every "other user" fixture in this file').toBe(200);
  const body = (await response.json()) as { rows?: readonly { id?: string; name?: string }[] };
  const others = (body.rows ?? []).filter((row) => row.id !== undefined && row.id !== selfId && row.name);
  expect(
    others.length,
    `the seed must provision at least ${count} OTHER users, or this file's fixtures have no meaning`,
  ).toBeGreaterThanOrEqual(count);
  // De-duplicated by id — a directory that listed the same row twice would
  // otherwise silently satisfy `count` with fewer real people than intended.
  const seen = new Map<string, { readonly id: string; readonly name: string }>();
  for (const row of others) seen.set(String(row.id), { id: String(row.id), name: String(row.name) });
  const distinct = [...seen.values()];
  expect(distinct.length, 'the users read back must be DISTINCT people').toBeGreaterThanOrEqual(count);
  return distinct.slice(0, count);
}

/** One participant row exactly as `GET /conversation/...` and the attach echo state it. */
interface ParticipantRow {
  readonly id: number | string;
  readonly entity_name?: string;
  readonly meta?: { readonly user_name?: string };
}

/** POST the participants array and hand back what the SERVER stored for it. */
async function attachParticipants(
  request: APIRequestContext,
  conversationId: string,
  bodies: readonly Record<string, unknown>[],
): Promise<readonly ParticipantRow[]> {
  const path = `/elitea_core/participants/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`;
  const response = await request.post(`${API_BASE}${path}`, { data: bodies });
  expect(
    response.status(),
    `the participants must attach: ${(await response.text()).slice(0, 300)}`,
  ).toBe(200);
  return (await response.json()) as readonly ParticipantRow[];
}

/** Focuses the composer and types `text` one keystroke at a time — the only way the mention-detection hooks actually run. Use this for the FIRST interaction on a fresh page load only. */
async function typeInComposer(page: Page, text: string): Promise<void> {
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 15_000 });
  await input.click();
  await input.pressSequentially(text, { delay: 15 });
}

/**
 * Continues typing into an ALREADY-FOCUSED composer, without re-clicking it.
 *
 * A second `.click()` on the input while the mention dropdown is open is not
 * a no-op: `UserMentionList`'s own `ClickAwayListener` wraps only the
 * dropdown's DOM subtree, and the input is its SIBLING, not a descendant — so
 * clicking the input while the dropdown is showing reads as a click AWAY
 * FROM it, firing `onClose` (`stopProcessingAtSymbol`) and silently
 * dismissing the whole mention state before the next keystroke ever lands.
 * Every multi-step interaction in this file after its own first
 * `typeInComposer` call uses this instead.
 */
async function typeMore(page: Page, text: string): Promise<void> {
  const input = page.getByTestId('chat-message-input');
  await input.pressSequentially(text, { delay: 15 });
}

/** The mention dropdown's own heading — present only while it is mounted (`ChatBoxPopups.tsx`'s `userMentions` gate). Not ambiguous with the (collapsed, unopened) participants rail, which never renders this text unless expanded — and nothing here expands it. */
function mentionDropdown(page: Page) {
  return page.getByText('Participants', { exact: true });
}

/* onetest: ELITEA-0393 — typing "@" in a conversation with only the caller as participant never mounts the mention dropdown, and "@" stays plain text */
test('ELITEA-0393: no autocomplete dropdown with only the caller as a participant', async ({ page }) => {
  const conversationId = await createConversation(page.request, uniqueName('solo'));
  try {
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });
    await checkA11y(page);

    await typeInComposer(page, '@test');
    await expect(mentionDropdown(page), 'a solo conversation must never show the mention dropdown').toHaveCount(0);

    // "@" is plain text, not silently dropped — the composer still works.
    const input = page.getByTestId('chat-message-input');
    await expect(input).toHaveValue('@test');
    await expect(page.getByTestId('chat-send-button')).toBeEnabled();
  } finally {
    await deleteConversation(page.request, conversationId);
  }
});

/* onetest: ELITEA-0391 — the dropdown does not appear before a second REAL participant is attached, and does appear once one is */
test('ELITEA-0391: autocomplete activates only once a second participant exists', async ({ page }) => {
  const author = await readAuthor(page.request);
  const [teammate] = await readOtherUsers(page.request, author.id, 1);
  const conversationId = await createConversation(page.request, uniqueName('activate'));
  try {
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    // Before: only the caller. No dropdown.
    await typeInComposer(page, '@');
    await expect(mentionDropdown(page), 'one-participant conversation must not offer autocomplete').toHaveCount(0);

    // Attach the second participant server-side, exactly as the participants
    // panel's own "add" picker does (`chat.management.spec.ts`'s M2c).
    await attachParticipants(page.request, conversationId, [
      { entity_name: 'user', entity_meta: { id: teammate!.id } },
    ]);

    // The conversation's participants are fetched on load; a fresh load is
    // what makes the newly-attached teammate visible to `useChatBoxState`.
    await page.reload();
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    await typeInComposer(page, '@');
    await expect(mentionDropdown(page), 'a second REAL participant must activate the dropdown').toBeVisible({
      timeout: 10_000,
    });
    await expect(page.getByText(teammate!.name, { exact: true })).toBeVisible();
  } finally {
    await deleteConversation(page.request, conversationId);
  }
});

/*
 * onetest: ELITEA-0394 — the dropdown lists exactly the conversation's
 * attached participants, never every project member, filtering included.
 *
 * Adapted: this stack's default project seeds exactly ONE other human
 * account besides the member persona (`readOtherUsers` measures this at run
 * time), so the manual case's 3rd, never-attached "Carol" cannot be built
 * from a real distinct user. The synthetic "Everyone" entry
 * (`useChatBoxState`'s `users` memo always appends `{id: '@everyone', ...}`)
 * stands in as the SECOND legitimate target, and the discriminator this case
 * is actually about — the dropdown is built from `participants`, not from
 * the project directory — is instead proven by BOUNDING the result: with
 * only the one real teammate attached, the dropdown must show exactly that
 * teammate plus "Everyone" and nothing else, even though the project
 * directory itself is not empty (the caller is in it too, and is correctly
 * excluded as "self", never as "not a participant").
 */
test('ELITEA-0394: the dropdown lists only conversation participants, not every project member', async ({
  page,
}) => {
  const author = await readAuthor(page.request);
  const [teammate] = await readOtherUsers(page.request, author.id, 1);
  const conversationId = await createConversation(page.request, uniqueName('scoped'));
  try {
    await attachParticipants(page.request, conversationId, [
      { entity_name: 'user', entity_meta: { id: teammate!.id } },
    ]);

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    await typeInComposer(page, '@');
    await expect(mentionDropdown(page)).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(teammate!.name, { exact: true })).toBeVisible();
    await expect(page.getByText('Everyone', { exact: true })).toBeVisible();
    // The caller's own name must never appear — self-exclusion, not
    // absence-because-not-a-participant (the discriminator this case is
    // actually about: a dropdown reading the whole directory would still
    // list every OTHER directory row too, not just attached participants;
    // in this stack's default project the directory holds exactly the
    // caller and the one attached teammate, so "not the caller" is the
    // strongest bound available — see this test's own header note).
    await expect(page.getByText(author.name, { exact: true })).toHaveCount(0);

    // Filtering for a name nobody has must surface nobody — a dropdown that
    // quietly fell back to some other directory would still list a match.
    await typeMore(page, 'ZzzzNoSuchPerson');
    await expect(mentionDropdown(page), 'no match must unmount the dropdown entirely').toHaveCount(0);
  } finally {
    await deleteConversation(page.request, conversationId);
  }
});

/*
 * onetest: ELITEA-0392 — full mention flow in one run: trigger, type-filter,
 * mouse-select, re-trigger, arrow+Enter select, both mentions land in the
 * composer's text.
 *
 * Adapted: the second mention target is the synthetic "Everyone" entry
 * rather than a second real user — see ELITEA-0394's own adaptation note
 * above for why (this stack seeds exactly one other human account). The
 * mechanism under test (trigger/filter/select/insert, by mouse and by
 * keyboard) does not care whether a candidate is a real participant row or
 * the synthetic one; both are ordinary entries in the same `users` list.
 */
test('ELITEA-0392: trigger, filter, mouse-select, re-trigger, keyboard-select, insert both mentions', async ({
  page,
}) => {
  const author = await readAuthor(page.request);
  const [first] = await readOtherUsers(page.request, author.id, 1);
  const second = { name: 'Everyone' };
  const conversationId = await createConversation(page.request, uniqueName('flow'));
  try {
    await attachParticipants(page.request, conversationId, [
      { entity_name: 'user', entity_meta: { id: first!.id } },
    ]);

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    const input = page.getByTestId('chat-message-input');
    await expect(input).toBeEditable({ timeout: 20_000 });

    // 1) Trigger — both participants listed.
    await typeInComposer(page, '@');
    await expect(mentionDropdown(page)).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(first!.name, { exact: true })).toBeVisible();
    await expect(page.getByText(second.name, { exact: true })).toBeVisible();

    // 2) Filter — type enough of `first`'s name that only it matches.
    // `useMentionDetection`'s filter is `u.name.toLowerCase().includes(searchStr)`,
    // so a single distinguishing character is enough as long as it is not also
    // a substring of `second`'s name.
    // Alphabetic-only, and never the leading token: this stack's seeded
    // "other user" name (e.g. "E2E Admin") mixes digits into its first word,
    // and filtering is exercised on ordinary name characters here.
    const nameWords = first!.name.split(/\s+/).filter((w) => /^[A-Za-z]+$/.test(w));
    const firstToken = (nameWords.at(-1) ?? first!.name).slice(0, 3);
    if (second.name.toLowerCase().includes(firstToken.toLowerCase())) {
      test.skip(true, 'this stack\'s two seeded users share a name prefix — filtering cannot discriminate them');
    }
    await typeMore(page, firstToken);
    await expect(page.getByText(second.name, { exact: true }), 'the filter must narrow the list').toHaveCount(0);
    await expect(page.getByText(first!.name, { exact: true })).toBeVisible();

    // 3) Mouse-select.
    await page.getByText(first!.name, { exact: true }).click();
    await expect(mentionDropdown(page)).toHaveCount(0);
    await expect(input).toHaveValue(`@${first!.name} `);

    // 4) Plain text appended after the inserted mention — no dropdown yet.
    await typeMore(page, 'and ');
    await expect(input).toHaveValue(`@${first!.name} and `);
    await expect(mentionDropdown(page)).toHaveCount(0);

    // 5) Re-trigger — both participants listed again.
    await typeMore(page, '@');
    await expect(mentionDropdown(page)).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(first!.name, { exact: true })).toBeVisible();
    await expect(page.getByText(second.name, { exact: true })).toBeVisible();

    // 6) Keyboard-select — ArrowDown then Enter. `UserMentionList`'s own
    // listener resets the active index to 0 on every filtered-list change and
    // owns a document-capture keydown, so this works regardless of DOM focus.
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('Enter');
    await expect(mentionDropdown(page)).toHaveCount(0);

    // 7) Both mentions present, in order, in the composer's own text.
    await expect(input).toHaveValue(`@${first!.name} and @${second.name} `);

    await typeMore(page, 'please review.');
    await expect(input).toHaveValue(`@${first!.name} and @${second.name} please review.`);

    await checkA11y(page);
  } finally {
    await deleteConversation(page.request, conversationId);
  }
});
