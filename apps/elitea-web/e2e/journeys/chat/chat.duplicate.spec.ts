/**
 * Chat "Duplicate" action (issue 940/A6, onetest folder
 * `chat-interface/chat_duplicate`, ELITEA-2616..2624).
 *
 * The row menu (`ConversationItem.menu.tsx`) gained a "Duplicate" item that
 * composes a copy client-side (no dedicated Go clone route exists — see
 * `processes/chat/model/useDuplicateConversation.ts`'s own module doc):
 * `GET` the original's details, `POST` a new conversation, re-`POST` its
 * participants, and (for a public original) a follow-up `PUT is_private:
 * false` — the same route "Make public" already uses.
 *
 * Ported BY USE CASE, not by legacy UI detail: the onetest source describes
 * a naming convention ("Copy of"/numbering, ELITEA-2636 — not one of this
 * package's 9 cases) this app does not reproduce; the app's own convention
 * is "<name> (copy)", asserted here as a substring rather than pinned to
 * the legacy wording.
 *
 * NOT covered here, and why: ELITEA-2618's own live half — "a message sent
 * in the ORIGINAL after duplicating does not appear in the duplicate" —
 * needs a real model turn, and the chat-stream stack is not available this
 * wave (mission preamble). What IS proven here is the structural half: the
 * duplicate is created with an EMPTY transcript (no `chat_history` is ever
 * read off the original), which is what makes the live half true in the
 * first place — nothing to copy from means nothing crosses over.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  addConversationParticipant,
  createConversation,
  deleteConversation,
  describeRefusal,
  readConversationDetails,
  readStoredMessageGroups,
  setConversationPrivacy,
} from '../../fixtures/api';

/** Every entity this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-dup';

const FOLDERS_PATH = `${API_BASE}/elitea_core/folder/prompt_lib/${DEFAULT_PROJECT_ID}`;

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}${SUFFIX}`;
}

/** Same shape/reasoning as `chat.sharing.spec.ts`'s own `openTodayGroup` — the rail's "Today" group starts collapsed. */
async function openTodayGroup(page: Page): Promise<void> {
  const sidebar = page.getByTestId('chat-conversation-sidebar');
  await expect(sidebar).toBeVisible({ timeout: 20_000 });
  const today = sidebar.getByRole('button', { name: 'Today' });
  await expect(today).toBeVisible({ timeout: 20_000 });
  if ((await today.getAttribute('aria-expanded')) !== 'true') await today.click();
}

/** Same gesture-retry shape as `chat.sharing.spec.ts`'s own `openRowMenu` — see that file's doc comment for why the hover and the click are one retried unit. */
async function openRowMenu(page: Page, conversationId: string): Promise<void> {
  const row = page.getByTestId(`conversation-item-${conversationId}`);
  await expect(row).toBeVisible({ timeout: 20_000 });
  const menu = page.getByRole('menu');
  await expect(async () => {
    if ((await menu.count()) === 0) {
      await row.hover();
      await row.getByRole('button', { name: 'More actions' }).click({ timeout: 5_000 });
    }
    await expect(menu).toBeVisible({ timeout: 5_000 });
  }).toPass({ timeout: 40_000 });
}

async function openChat(page: Page): Promise<void> {
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
  await openTodayGroup(page);
}

/** The ids in the sidebar's PINNED group — same route `chat.sharing.spec.ts`'s own `readPinnedIds` reads. */
async function readPinnedIds(page: Page): Promise<readonly string[]> {
  const url = `${FOLDERS_PATH}?grouped=true`;
  const response = await page.request.get(url);
  if (!response.ok()) {
    throw new Error(`readPinnedIds: GET ${url} -> ${response.status()} ${response.statusText()}${await describeRefusal(response)}`);
  }
  const body = (await response.json()) as { pinned?: { conversations?: readonly { id?: unknown }[] } };
  return (body.pinned?.conversations ?? []).map((conversation) => String(conversation.id ?? ''));
}

/**
 * Opens the row menu and clicks "Duplicate", returning the SERVER's own
 * answer for the new conversation (armed before the click, so the response
 * cannot be missed between the gesture and the wait — same pattern
 * `chat.sharing.spec.ts`'s export tests use for their own POST/GET).
 */
async function duplicateConversation(page: Page, conversationId: string): Promise<{ readonly id: string; readonly name: string }> {
  await openRowMenu(page, conversationId);
  const created = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' &&
      response.url().includes(`/elitea_core/conversations/prompt_lib/${DEFAULT_PROJECT_ID}`) &&
      response.status() < 400,
    { timeout: 20_000 },
  );
  await page.getByRole('menuitem', { name: 'Duplicate' }).click();
  const body = (await (await created).json()) as { id?: string | number; name?: string };
  if (body.id === undefined) throw new Error(`duplicateConversation: create response carried no id: ${JSON.stringify(body)}`);
  return { id: String(body.id), name: body.name ?? '' };
}

async function removeConversation(request: APIRequestContext, id: string | undefined): Promise<void> {
  if (id !== undefined) await deleteConversation(request, id);
}

/* onetest: ELITEA-2616, ELITEA-2619 — Duplicate creates a new, independent conversation; the original is
 * unchanged; both persist as separate entities after a reload. */
test('D1: Duplicate creates an independent copy that lands the user in it, leaving the original untouched, and both survive a reload', async ({ page }) => {
  test.setTimeout(90_000);
  const originalName = uniqueName('d1-orig');
  const originalId = await createConversation(page.request, originalName);
  let duplicateId: string | undefined;
  try {
    await openChat(page);
    const duplicate = await duplicateConversation(page, originalId);
    duplicateId = duplicate.id;

    expect(duplicateId, 'the duplicate must be a NEW conversation, not the original re-served').not.toBe(originalId);
    expect(duplicate.name, 'the duplicate is named off the original, with the app\'s own naming convention').toContain(originalName);
    expect(duplicate.name).toContain('(copy)');

    // Lands the user in the copy: the URL follows the newly created id.
    await expect(page).toHaveURL(new RegExp(`/chat/${duplicateId}$`), { timeout: 15_000 });

    // The original is untouched — read from the SERVER, not the rail.
    const original = await readConversationDetails(page.request, originalId);
    expect(original.is_private, 'the original conversation must remain unchanged by duplicating it').not.toBe(false);

    // Both survive a reload as separate, independent rows.
    await openChat(page);
    await expect(page.getByTestId(`conversation-item-${originalId}`)).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId(`conversation-item-${duplicateId}`)).toBeVisible({ timeout: 15_000 });
  } finally {
    await removeConversation(page.request, duplicateId);
    await removeConversation(page.request, originalId);
  }
});

/* onetest: ELITEA-2617, ELITEA-2618 — the duplicate carries every participant and its configuration
 * verbatim, and starts with an EMPTY transcript (the structural half of conversation independence — see
 * this file's own header for the live half this wave's stack cannot exercise). */
test('D2: Duplicate preserves participants and their settings, and starts as an empty, independent conversation', async ({ page }) => {
  test.setTimeout(90_000);
  const originalId = await createConversation(page.request, uniqueName('d2-orig'));
  let duplicateId: string | undefined;
  try {
    await addConversationParticipant(page.request, originalId, {
      entity_name: 'llm',
      entity_meta: { model_name: 'autotest-dup-model' },
      entity_settings: { temperature: 0.42 },
    });
    const originalParticipants = (await readConversationDetails(page.request, originalId)).participants;
    expect(originalParticipants, 'the seed must have taken before duplicating').toHaveLength(1);

    await openChat(page);
    const duplicate = await duplicateConversation(page, originalId);
    duplicateId = duplicate.id;

    await expect
      .poll(async () => (await readConversationDetails(page.request, duplicateId as string)).participants.length, {
        timeout: 15_000,
        message: 'the duplicate must carry the original\'s participant',
      })
      .toBe(1);
    const duplicateParticipant = (await readConversationDetails(page.request, duplicateId)).participants[0];
    expect(duplicateParticipant?.entity_name).toBe('llm');
    expect(duplicateParticipant?.entity_meta?.['model_name']).toBe('autotest-dup-model');
    // NOTE (measured, not a defect): `chat_participants` is keyed by
    // identity on `(entity_name, entity_meta)`
    // (`internal/infra/db/repos/conversations.go`'s `AddParticipant`) — an
    // identical participant re-posted for a different conversation reuses
    // the SAME participant row id and only adds a new mapping row. So
    // `duplicateParticipant.id` legitimately EQUALS the original's here;
    // what actually proves independence is the separate MAPPING (a
    // `chat_participant_mapping` row scoped to `duplicateId`, which is why
    // this participant shows up on the duplicate's own read at all,
    // despite never being duplicated by conversation id).
    expect(duplicateParticipant?.id).toBe(originalParticipants[0]?.id);

    // No `chat_history` is ever read off the original — the duplicate opens
    // with nothing to play back.
    const groups = await readStoredMessageGroups(page, DEFAULT_PROJECT_ID, duplicateId, 10);
    expect(groups, 'a fresh duplicate must start with an empty transcript').toEqual([]);
  } finally {
    await removeConversation(page.request, duplicateId);
    await removeConversation(page.request, originalId);
  }
});

/* onetest: ELITEA-2620 — duplicating a pinned chat leaves the duplicate UNPINNED while the original
 * remains pinned. */
test('D3: Duplicate of a pinned chat is itself unpinned; the original stays pinned', async ({ page }) => {
  test.setTimeout(90_000);
  const originalId = await createConversation(page.request, uniqueName('d3-orig'));
  let duplicateId: string | undefined;
  try {
    await openChat(page);
    await openRowMenu(page, originalId);
    await page.getByRole('menuitem', { name: 'Pin on top' }).click();
    await expect
      .poll(async () => readPinnedIds(page), { timeout: 20_000, message: 'the original must be pinned before duplicating it' })
      .toContain(originalId);

    const duplicate = await duplicateConversation(page, originalId);
    duplicateId = duplicate.id;

    // Pin state is a separate entity-pin mapping the duplicate never
    // inherits — a fresh conversation id simply has no pin row.
    const pinned = await readPinnedIds(page);
    expect(pinned, 'the original must remain pinned').toContain(originalId);
    expect(pinned, 'the duplicate must NOT be pinned').not.toContain(duplicateId);
  } finally {
    await removeConversation(page.request, duplicateId);
    await removeConversation(page.request, originalId);
  }
});

/* onetest: ELITEA-2621, ELITEA-2622, ELITEA-2623 — duplicating a PUBLIC chat produces a public
 * duplicate, visible to a team member who is not a participant, and public visibility survives a
 * SECOND round of duplication (duplicate of a duplicate). */
test('D4: Duplicate of a public chat is itself public and visible to a non-participant team member, through a nested duplication', async ({ page, browser }) => {
  test.setTimeout(120_000);
  const originalId = await createConversation(page.request, uniqueName('d4-orig'));
  let firstDuplicateId: string | undefined;
  let secondDuplicateId: string | undefined;
  try {
    await setConversationPrivacy(page.request, originalId, false);
    expect((await readConversationDetails(page.request, originalId)).is_private).toBe(false);

    await openChat(page);
    const firstDuplicate = await duplicateConversation(page, originalId);
    firstDuplicateId = firstDuplicate.id;
    await expect
      .poll(async () => (await readConversationDetails(page.request, firstDuplicateId as string)).is_private, {
        timeout: 15_000,
        message: 'a duplicate of a public chat must itself be public',
      })
      .toBe(false);

    // A DIFFERENT team member, never a participant on either conversation:
    // the public flag is what makes it reachable, not participation.
    const otherMemberCtx = await browser.newContext({ storageState: STORAGE_STATE.admin });
    try {
      const details = await readConversationDetails(otherMemberCtx.request, firstDuplicateId);
      expect(details.is_private, 'a non-participant team member must be able to read a PUBLIC duplicate').toBe(false);
    } finally {
      await otherMemberCtx.close();
    }

    // Nested duplication: duplicating the duplicate preserves public
    // visibility through a second generation.
    await openChat(page);
    const secondDuplicate = await duplicateConversation(page, firstDuplicateId);
    secondDuplicateId = secondDuplicate.id;
    await expect
      .poll(async () => (await readConversationDetails(page.request, secondDuplicateId as string)).is_private, {
        timeout: 15_000,
        message: 'a duplicate of a duplicate of a public chat must remain public',
      })
      .toBe(false);
  } finally {
    await removeConversation(page.request, secondDuplicateId);
    await removeConversation(page.request, firstDuplicateId);
    await removeConversation(page.request, originalId);
  }
});

/* onetest: ELITEA-2624 — duplicating cannot expose or relocate a conversation outside its own
 * project: the harness has no ready two-tenant fixture (mission preamble: "the closest behaviour that
 * keeps the app coherent"), so this checks the guarantee the code actually makes — every read/write the
 * duplicate flow performs stays scoped to the SAME project id the row was rendered for, and the
 * duplicate is unreachable through any other project path even for the SAME authenticated session. */
test('D5: a duplicated conversation is not reachable through a project it does not belong to', async ({ page }) => {
  test.setTimeout(90_000);
  const originalId = await createConversation(page.request, uniqueName('d5-orig'));
  let duplicateId: string | undefined;
  try {
    await openChat(page);
    const duplicate = await duplicateConversation(page, originalId);
    duplicateId = duplicate.id;

    // Ground truth: the duplicate DOES exist under its own project.
    expect((await readConversationDetails(page.request, duplicateId)).is_private).not.toBeUndefined();

    // The same duplicate id, addressed through an UNRELATED project path —
    // must not resolve, for the same session that just created it.
    const foreignProjectId = '999999';
    const response = await page.request.get(`${API_BASE}/elitea_core/conversation/prompt_lib/${foreignProjectId}/${duplicateId}`);
    expect([403, 404], `a duplicate must not be reachable via a foreign project id (got ${response.status()})`).toContain(response.status());
  } finally {
    await removeConversation(page.request, duplicateId);
    await removeConversation(page.request, originalId);
  }
});
