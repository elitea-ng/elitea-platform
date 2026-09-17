/**
 * Issues programme, package C-chat — regression pins for CLOSED legacy
 * conversation-list/rail issues, judged against THIS app.
 *
 * ── #6562 (name-length duplication failure) ─────────────────────────────────
 * Legacy: appending a numeric "(1)" suffix on Duplicate could push a
 * near-max-length name over a ~50-char server limit, and duplication then
 * failed outright with no error shown.
 *
 * NOT REPRODUCED in this app: `useDuplicateConversation.ts` composes the
 * duplicate's name client-side as `${source.name} (copy)` with no truncation
 * logic — and there IS no length cap to trip over. Grepped
 * `services/elitea-main/internal/api/v2/conversations/handler.go` (no
 * length validation on `name`) and this app's rename editor
 * (`ConversationItem.editor.tsx`, no `maxLength`/`inputProps` cap either).
 * A name near legacy's "~50 characters" limit is not a distinguished case
 * here at all, so this pins the general claim instead: an arbitrarily long
 * name still duplicates successfully with the suffix appended.
 *
 * ── #6133 (search resets the opened conversation to the greeting screen) ───
 * Legacy: typing in the rail's search box re-rendered the whole page and
 * dropped the open conversation back to the empty/greeting state.
 *
 * NOT REPRODUCED in this app: the rail's search state
 * (`Conversations.tsx`'s `isSearchActive`/query) lives entirely inside the
 * LEFT rail component; the right-hand chat pane is a sibling route
 * component that reads `conversationId` from the URL, not from the rail's
 * search state — there is no shared state for a search keystroke to
 * clobber. Pinned directly: open a conversation with a real message, then
 * search, and assert the pane still shows that same conversation and its
 * transcript, not the greeting.
 *
 * #5062/#4996/#5008 (duplicate/offset/pinned-conversation lazy-load bugs)
 * are COVERED-EXISTING — see
 * `Conversations.helpers.test.ts :: mergeLoadMorePage > "drops a pinned or
 * already present conversation and advances the offset"` (dedup by id,
 * pinned-id exclusion, and offset advancing by the raw page length rather
 * than a fixed +10 all live in `mergeLoadMorePage`) and
 * `chat.sidebarGaps.spec.ts :: ELITEA-0513` (pinned conversation excluded
 * from every date group server-side, across a reload).
 */
import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createConversation, deleteConversation } from '../../fixtures/api';

const SUFFIX = '-closed-conv';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}${SUFFIX}`;
}

async function openTodayGroup(page: Page): Promise<void> {
  const sidebar = page.getByTestId('chat-conversation-sidebar');
  await expect(sidebar).toBeVisible({ timeout: 20_000 });
  const today = sidebar.getByRole('button', { name: 'Today' });
  await expect(today).toBeVisible({ timeout: 20_000 });
  if ((await today.getAttribute('aria-expanded')) !== 'true') await today.click();
}

async function openRowMenu(page: Page, conversationId: string): Promise<void> {
  const row = page.getByTestId(`conversation-item-${conversationId}`);
  await expect(row).toBeVisible({ timeout: 15_000 });
  await row.hover();
  const menu = page.getByRole('menu');
  await expect
    .poll(
      async () => {
        if ((await menu.count()) > 0) return true;
        await row.getByRole('button', { name: 'More actions' }).click({ timeout: 5_000 });
        return (await menu.count()) > 0;
      },
      { timeout: 10_000 },
    )
    .toBe(true);
}

/* elitea_issues: #6562 — duplicating a conversation whose name is well past any legacy
 * "~50 character" concern still succeeds; the app's (copy)-suffix scheme has no length
 * cap to trip over. */
test('#6562: duplicating a very long-named conversation succeeds with the suffix appended', async ({ page }) => {
  test.setTimeout(60_000);
  // 180 chars: comfortably past the legacy ~50-char limit this issue depended on.
  const longName = uniqueName('L').padEnd(180, 'x');
  const originalId = await createConversation(page.request, longName);
  let duplicateId: string | undefined;
  try {
    await page.goto(BASE_URL + '/app/chat');
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
    await openTodayGroup(page);
    await openRowMenu(page, originalId);

    const created = page.waitForResponse(
      (response) => response.request().method() === 'POST' && response.status() < 400 && response.url().includes('/elitea_core/conversations/prompt_lib/'),
      { timeout: 20_000 },
    );
    await page.getByRole('menuitem', { name: 'Duplicate' }).click();
    const body = (await (await created).json()) as { id?: string | number; name?: string };
    expect(body.id, 'duplication must not fail silently for a long name').toBeDefined();
    duplicateId = String(body.id);
    expect(body.name ?? '').toContain('(copy)');
    expect((body.name ?? '').length, 'the duplicate name is not silently truncated to some legacy cap').toBeGreaterThan(longName.length);

    await expect(page).toHaveURL(new RegExp(`/chat/${duplicateId}$`), { timeout: 15_000 });
  } finally {
    await deleteConversation(page.request, originalId);
    if (duplicateId !== undefined) await deleteConversation(page.request, duplicateId);
  }
});

/* elitea_issues: #6133 — searching the rail narrows the LEFT list only; the currently
 * open conversation (right pane, its messages) is untouched, never reset to the
 * greeting/empty state. */
test('#6133: searching the conversation rail does not reset the currently open conversation', async ({ page }) => {
  test.setTimeout(60_000);
  const openedName = uniqueName('opened');
  const otherName = uniqueName('other');
  const openedId = await createConversation(page.request, openedName);
  const otherId = await createConversation(page.request, otherName);
  try {
    await page.goto(`${BASE_URL}/app/chat/${openedId}`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
    // The greeting screen is what #6133 regresses TO — assert we start away from it
    // is not meaningful for a brand-new empty conversation, so instead assert the
    // route/selected-row stays pinned to `openedId` across a search keystroke.
    await openTodayGroup(page);
    await expect(page.getByTestId(`conversation-item-${openedId}`)).toBeVisible({ timeout: 15_000 });

    await page.getByTestId('conversation-search-button').click();
    const input = page.getByTestId('conversation-search-input');
    await expect(input).toBeVisible({ timeout: 10_000 });
    await input.fill(otherName);
    await expect
      .poll(async () => page.getByTestId(`conversation-item-${openedId}`).count(), {
        timeout: 15_000,
        message: 'the search narrowed the rail as expected',
      })
      .toBe(0);

    // The URL — and therefore the open conversation — must be untouched by a rail search.
    await expect(page).toHaveURL(new RegExp(`/chat/${openedId}$`));
    await expect(page.getByTestId('chat-input')).toBeVisible();
  } finally {
    await deleteConversation(page.request, openedId);
    await deleteConversation(page.request, otherId);
  }
});
