/**
 * Sharing a conversation: publishing it inside the project, publishing it to a
 * reader with no account at all, pinning it, and the export control.
 *
 * Ported by USE CASE from the legacy suite's conversation-visibility cases
 * (make private / share with the team / make public) — no legacy file, test or
 * identifier is named here, only the behaviour.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS FILE EXISTS AT ALL
 * ─────────────────────────────────────────────────────────────────────────────
 * Every control it drives was already rendered, and not one of them was
 * reachable from any test. That combination is what let three of them be
 * gestures with no effect, each in a different layer, and none of the three
 * announced itself:
 *
 *  1. MAKE PUBLIC PUT `is_private: false` at a handler that read `name`,
 *     `folder_id` and `meta` and discarded everything else. The column was set
 *     TRUE by the INSERT and never written again. The confirm dialog closed,
 *     the client patched its own list, and a reload put the conversation back.
 *  2. PIN wrote `social_pins` — and the sidebar listing read
 *     `meta->>'is_pinned'`, a key nothing writes for a conversation. The row
 *     jumped to the pinned group optimistically and the next listing put it
 *     back, forever.
 *  3. EXPORT is a stub to this day: the menu entry is disabled and its two
 *     options carry placeholder labels wired to nothing. It is asserted here
 *     AS a stub — see the last test — so that the day it is implemented this
 *     assertion fails and has to be rewritten, rather than a missing feature
 *     going on looking like a passing suite.
 *
 * The first two are fixed in the same change as this file; each is also pinned
 * by a Go integration test at the layer that was wrong. What this journey adds
 * is the only statement neither of those can make: that the gesture a user
 * actually performs reaches the store.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE READER WITH NO SESSION
 * ─────────────────────────────────────────────────────────────────────────────
 * The share-by-link half is the one failure mode CI could never see, because
 * it is only visible to a SECOND person: a link that 404s for its recipient
 * looks perfect to the person who created it. The recipient is modelled with a
 * context of its own (`browser.newContext({ storageState: undefined })`) — not
 * by signing anything out. The suite's personas share one server-side session
 * and a logout revokes it for every worker, so a journey that ended a session
 * to prove anonymity would take the rest of the run with it.
 *
 * Every conversation here is created through the API before the UI is opened
 * and deleted through `page.request` (the browser context's session) after, and
 * every name carries `autotest_` and this file's own `-share` tag.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createConversation,
  deleteConversation,
  describeRefusal,
} from '../../fixtures/api';

/** Every entity this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-share';

const CONVERSATION_PATH = `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;
const FOLDERS_PATH = `${API_BASE}/elitea_core/folder/prompt_lib/${DEFAULT_PROJECT_ID}`;
const SHARE_LINKS_PATH = `${API_BASE}/elitea_core/shared_chat_links/prompt_lib/${DEFAULT_PROJECT_ID}`;

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}${SUFFIX}`;
}

/**
 * `is_private` as the SERVER holds it.
 *
 * Read from the conversation's own details route rather than from the sidebar
 * listing, so the assertion cannot be satisfied by a listing that happens to
 * carry a stale copy. Throws on a non-2xx: a refusal returned as "still
 * private" would read exactly like a publish that did not take.
 */
async function readIsPrivate(page: Page, conversationId: string): Promise<boolean> {
  const url = `${CONVERSATION_PATH}/${conversationId}`;
  const response = await page.request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readIsPrivate: GET ${url} -> ${response.status()} ${response.statusText()}${await describeRefusal(response)}`,
    );
  }
  const body = (await response.json()) as { is_private?: unknown };
  return body.is_private !== false;
}

/**
 * The ids in the sidebar's PINNED group, as the sidebar itself asks for them.
 *
 * `grouped=true` and no `limit`: this route pages each date group, never the
 * pinned one, and the question here is membership of a set the server builds.
 */
async function readPinnedIds(page: Page): Promise<readonly string[]> {
  const url = `${FOLDERS_PATH}?grouped=true`;
  const response = await page.request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readPinnedIds: GET ${url} -> ${response.status()} ${response.statusText()}${await describeRefusal(response)}`,
    );
  }
  const body = (await response.json()) as { pinned?: { conversations?: readonly { id?: unknown }[] } };
  return (body.pinned?.conversations ?? []).map((conversation) => String(conversation.id ?? ''));
}

/** One share link, as its owner's listing serves it. */
interface ShareLinkRow {
  readonly id: number;
  readonly active: boolean;
  readonly access_count: number;
}

async function readShareLinks(page: Page, conversationId: string): Promise<readonly ShareLinkRow[]> {
  const url = `${SHARE_LINKS_PATH}/${conversationId}`;
  const response = await page.request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readShareLinks: GET ${url} -> ${response.status()} ${response.statusText()}${await describeRefusal(response)}`,
    );
  }
  return (await response.json()) as readonly ShareLinkRow[];
}

/**
 * Opens the rail's "Today" group, which starts collapsed.
 *
 * Driven off `aria-expanded` rather than clicked unconditionally, so a future
 * default of "expanded" closes nothing. (Same helper and same reasoning as the
 * management and list journeys next door.)
 */
async function openTodayGroup(page: Page): Promise<void> {
  const sidebar = page.getByTestId('chat-conversation-sidebar');
  await expect(sidebar).toBeVisible({ timeout: 20_000 });
  const today = sidebar.getByRole('button', { name: 'Today' });
  await expect(today).toBeVisible({ timeout: 20_000 });
  if ((await today.getAttribute('aria-expanded')) !== 'true') await today.click();
}

/**
 * Opens one row's ⋮ menu.
 *
 * The trigger is mounted only while the row is HOVERED (`menuWrapper` is
 * `display: none` otherwise), so the hover is part of the gesture rather than
 * decoration — without it the click fails on an invisible element, which reads
 * like a missing control.
 */
async function openRowMenu(page: Page, conversationId: string): Promise<void> {
  const row = page.getByTestId(`conversation-item-${conversationId}`);
  await expect(row).toBeVisible({ timeout: 20_000 });
  await row.hover();
  await row.getByRole('button', { name: 'More actions' }).click();
  await expect(page.getByRole('menu')).toBeVisible({ timeout: 10_000 });
}

async function closeRowMenu(page: Page): Promise<void> {
  await page.keyboard.press('Escape');
  await expect(page.getByRole('menu')).toHaveCount(0, { timeout: 10_000 });
}

/** Opens the chat surface with the rail's today group expanded. */
async function openChat(page: Page): Promise<void> {
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
  await openTodayGroup(page);
}

async function removeConversation(request: APIRequestContext, id: string): Promise<void> {
  await deleteConversation(request, id);
}

// ─────────────────────────────────────────────────────────────────────────────
// legacy use case: a conversation's visibility — private → public
// ─────────────────────────────────────────────────────────────────────────────
test('S1: "Make public" publishes the conversation on the server, and the menu stops offering it', async ({ page }) => {
  /*
   * THE BUDGET IS THE SUM OF THE WAITS BELOW, not a round number: two full
   * loads of the chat page at 20 s each, a 20 s server poll between them, and
   * the menu waits either side. The default 30 s is smaller than that sum, so
   * the clock — not any assertion — decided the outcome, and the report named
   * the cleanup call that happened to be running when it ran out
   * (`apiRequestContext.delete`, measured on chromium).
   */
  test.setTimeout(120_000);
  const conversationId = await createConversation(page.request, uniqueName('public'));
  try {
    expect(await readIsPrivate(page, conversationId), 'a new conversation must start private').toBe(true);

    await openChat(page);
    await openRowMenu(page, conversationId);

    // The item, then its inline confirmation. The confirm REPLACES the row it
    // belongs to, so the second locator resolves to the confirm button rather
    // than to the item that opened it.
    await page.getByRole('menuitem', { name: 'Make public' }).click();
    await expect(page.getByText('Are you sure to make your conversation public?')).toBeVisible({ timeout: 10_000 });
    await page.getByRole('menuitem', { name: 'Make public' }).click();

    // THE SERVER, not the rail. The client patches its own list either way,
    // so a UI-only assertion here passes on the defect this closes.
    await expect
      .poll(async () => readIsPrivate(page, conversationId), {
        timeout: 20_000,
        message: 'the conversation must be published on the server, not only in the list the client is holding',
      })
      .toBe(false);

    // And the whole chain, end to end: after a reload the SIDEBAR LISTING is
    // what tells the menu this conversation is already public, so the control
    // that publishes it is no longer offered. This fails if the listing does
    // not carry `is_private`, which is where the second half of the defect was.
    await openChat(page);
    await openRowMenu(page, conversationId);
    await expect(
      page.getByRole('menuitem', { name: 'Make public' }),
      'a published conversation must not still offer "Make public"',
    ).toHaveCount(0);
    await closeRowMenu(page);
  } finally {
    await removeConversation(page.request, conversationId);
  }
});

// ─────────────────────────────────────────────────────────────────────────────
// legacy use case: share a conversation by link, and revoke the link
// ─────────────────────────────────────────────────────────────────────────────
test('S2: a share link opens for a reader with no session, and dies when it is revoked', async ({ page, browser }) => {
  // Same arithmetic as S1's, over more waits: the owner's chat page, the
  // dialog, the reader's own context and its two page loads, and two 20 s
  // server polls.
  test.setTimeout(150_000);
  const name = uniqueName('link');
  const conversationId = await createConversation(page.request, name);
  try {
    await openChat(page);
    await openRowMenu(page, conversationId);
    await page.getByRole('menuitem', { name: 'Share by link' }).click();

    const dialog = page.getByTestId('share-link-dialog');
    await expect(dialog).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId('share-link-empty'), 'a fresh conversation has no links').toBeVisible();

    await page.getByTestId('share-link-create').click();

    // The URL is shown exactly once — the server stores only a hash of the
    // token — so it is read off the screen here rather than from any listing,
    // which is the whole point of that design.
    const created = page.getByTestId('share-link-created');
    await expect(created).toBeVisible({ timeout: 15_000 });
    const shareUrl = ((await created.textContent()) ?? '').match(/https?:\/\/\S*\/shared\/chat\/[A-Za-z0-9_-]+/)?.[0] ?? '';
    expect(shareUrl, 'the dialog must show the one-shot link it just created').not.toBe('');

    const links = await readShareLinks(page, conversationId);
    expect(links, 'the server must hold exactly the one link the dialog created').toHaveLength(1);
    const linkId = links[0]?.id ?? 0;
    expect(links[0]?.active).toBe(true);

    // THE RECIPIENT. A context of its own, with no storage state: this is the
    // half nothing proved, and the half a link that 404s only ever fails in.
    const reader = await browser.newContext({ storageState: undefined });
    try {
      const readerPage = await reader.newPage();
      await readerPage.goto(shareUrl);
      await expect(
        readerPage.getByTestId('shared-conversation'),
        'a link holder with no session must be able to read the conversation',
      ).toBeVisible({ timeout: 20_000 });
      await expect(readerPage.getByText(name)).toBeVisible({ timeout: 10_000 });
      // Seeded through the API and never spoken in, so the transcript is
      // empty — the page must say so rather than render nothing at all.
      await expect(readerPage.getByTestId('shared-conversation-empty')).toBeVisible();

      // The server counted the visit. This is the owner-facing evidence that
      // the anonymous read really reached the store, independent of what the
      // reader's page rendered.
      await expect
        .poll(async () => (await readShareLinks(page, conversationId))[0]?.access_count ?? 0, {
          timeout: 20_000,
          message: 'the link must record the anonymous read',
        })
        .toBeGreaterThan(0);

      // REVOKE, from the same dialog the owner still has open.
      await page.getByTestId('share-link-revoke').click();
      await expect
        .poll(async () => (await readShareLinks(page, conversationId)).find((link) => link.id === linkId)?.active ?? true, {
          timeout: 20_000,
          message: 'the revoked link must be inactive on the server',
        })
        .toBe(false);

      // …and the recipient's page stops working, which is the only statement
      // that matters to the person who revoked it.
      await readerPage.reload();
      await expect(
        readerPage.getByTestId('shared-conversation-unavailable'),
        'a revoked link must stop opening the conversation',
      ).toBeVisible({ timeout: 20_000 });
    } finally {
      await reader.close();
    }
  } finally {
    await removeConversation(page.request, conversationId);
  }
});

// ─────────────────────────────────────────────────────────────────────────────
// legacy use case: pin a conversation to the top of the rail, and unpin it
// ─────────────────────────────────────────────────────────────────────────────
test('S3: pin moves the conversation into the sidebar\'s pinned group on the server, and unpin takes it out', async ({ page }) => {
  // The same arithmetic S1 states: a chat page load and two 20 s server polls
  // do not fit in the 30 s default.
  test.setTimeout(120_000);
  const conversationId = await createConversation(page.request, uniqueName('pin'));
  try {
    expect(await readPinnedIds(page)).not.toContain(conversationId);

    await openChat(page);
    await openRowMenu(page, conversationId);
    await page.getByRole('menuitem', { name: 'Pin on top' }).click();

    // The rail moves the row optimistically whatever happens, so the listing
    // the NEXT reader gets is the only honest measurement.
    await expect
      .poll(async () => readPinnedIds(page), {
        timeout: 20_000,
        message: 'a pinned conversation must be in the pinned group the sidebar listing builds',
      })
      .toContain(conversationId);

    await openRowMenu(page, conversationId);
    await page.getByRole('menuitem', { name: 'Unpin' }).click();
    await expect
      .poll(async () => readPinnedIds(page), {
        timeout: 20_000,
        message: 'an unpinned conversation must leave the pinned group',
      })
      .not.toContain(conversationId);
  } finally {
    await removeConversation(page.request, conversationId);
  }
});

// ─────────────────────────────────────────────────────────────────────────────
// legacy use case: export a conversation
//
// STATED AS THE STUB IT IS. The legacy product exports a conversation; this one
// renders the entry, disables it, and offers two placeholder options wired to
// no handler at all. Asserting the disabled state is not coverage of export —
// it is a record of the gap that FAILS the day export is implemented, which is
// the only way a missing feature stops reading like a passing suite.
// ─────────────────────────────────────────────────────────────────────────────
test('S4: the Export entry is present but does nothing — the port has no conversation export', async ({ page }) => {
  // As S1: measured running out of the 30 s default in the cleanup call, which
  // is a report about the clock rather than about the export entry.
  test.setTimeout(120_000);
  const conversationId = await createConversation(page.request, uniqueName('export'));
  try {
    await openChat(page);
    await openRowMenu(page, conversationId);

    const exportItem = page.getByRole('menuitem', { name: 'Export' });
    await expect(exportItem).toBeVisible({ timeout: 10_000 });
    await expect(exportItem, 'export is not implemented; the entry must not pretend otherwise').toHaveAttribute(
      'aria-disabled',
      'true',
    );

    // A disabled parent cannot be opened, so its two placeholder options are
    // unreachable — the second half of "there is no export here".
    await exportItem.click({ force: true });
    await expect(page.getByRole('menuitem', { name: 'Option1' })).toHaveCount(0);
    await closeRowMenu(page);
  } finally {
    await removeConversation(page.request, conversationId);
  }
});
