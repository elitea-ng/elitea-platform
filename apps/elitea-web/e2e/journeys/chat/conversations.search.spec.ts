/**
 * Searching the sidebar's Chats list.
 *
 * Ported by use case from the legacy public suite. TWO legacy tests describe
 * the same use case from two files, and one journey closes both:
 *
 *  - `tests/ui/chat/test_conversation_management.py::TestConversationList::test_search_conversations_button`
 *    (TC-CONV-005)
 *  - `tests/ui/chat/test_chat_interface.py::TestSearchAndErrorHandling::test_search_conversations_dialog`
 *    (TC-CHAT-023)
 *
 * Both of them stopped at "an input appeared". That is the half of the feature
 * that cannot be wrong in an interesting way — the Chats caption's search
 * button (`shell-chats-search`) opens `ChatsSearchField`. The half that can,
 * and the half a user notices, is whether typing NARROWS the list: the rail's
 * filter (`widgets/chat-rail/model/chatRail.tsx`) debounces the query onto the
 * grouped-listing read, and the server answers with the matching rows. A
 * search box wired to no query looks exactly like a working one until you
 * type. So this journey types, and asserts both directions: the row that
 * matches stays, the row that does not goes.
 *
 * The clear path is asserted too: Escape empties the query and closes the
 * field, and the full list comes back.
 */
import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createConversation, deleteConversation } from '../../fixtures/api';

/** Every entity this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-search';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}${SUFFIX}`;
}

/** Opens the sidebar's Today group (open by default; a remembered close is undone). */
async function openTodayGroup(page: Page): Promise<void> {
  const list = page.getByTestId('shell-chats');
  await expect(list).toBeVisible({ timeout: 20_000 });
  const today = list
    .getByTestId('shell-chats-group')
    .filter({ has: page.locator('.shell-group-caption', { hasText: /^Today$/ }) })
    .locator('.shell-row-main');
  await expect(today).toBeVisible({ timeout: 20_000 });
  if ((await today.getAttribute('aria-expanded')) !== 'true') await today.click();
}

test('the rail’s search control narrows the list to the conversation it names', async ({ page }) => {
  const keptName = uniqueName('kept');
  const droppedName = uniqueName('dropped');
  const kept = await createConversation(page.request, keptName);
  const dropped = await createConversation(page.request, droppedName);

  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
  await openTodayGroup(page);

  const keptRow = page.locator(`[data-conversation-id="${kept}"]`);
  const droppedRow = page.locator(`[data-conversation-id="${dropped}"]`);
  await expect(keptRow).toBeVisible({ timeout: 15_000 });
  await expect(droppedRow).toBeVisible({ timeout: 15_000 });

  // The control is not an input until it is asked to be — the list renders
  // the search field only while the caption's search is open.
  const input = page.getByTestId('shell-chats-search-input');
  await expect(input).toHaveCount(0);
  await page.getByTestId('shell-chats-search').click();
  await expect(input).toBeVisible({ timeout: 10_000 });
  await expect(input).toBeEditable();

  await checkA11y(page);

  await input.fill(keptName);

  // Polled, because the query is debounced and then travels to the server
  // before the list re-renders. `count()` states the absence — the row
  // that no longer matches is unmounted, not merely hidden, so a visibility
  // assertion would wait for something that never attaches again.
  await expect
    .poll(async () => droppedRow.count(), {
      timeout: 15_000,
      message: 'a search that does not narrow the listing is a search box wired to nothing',
    })
    .toBe(0);
  // …and the match is still there, so "narrowed" is not "emptied".
  await expect(keptRow).toBeVisible();
  await expect(keptRow).toContainText(keptName);

  // Escape empties the query and closes the field, and the full list comes
  // back — including the row the query had removed.
  await input.press('Escape');
  await expect(input).toHaveCount(0);
  await openTodayGroup(page);
  await expect(droppedRow).toBeVisible({ timeout: 15_000 });
  await expect(keptRow).toBeVisible();

  await deleteConversation(page.request, kept);
  await deleteConversation(page.request, dropped);
});
