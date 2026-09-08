/**
 * The conversation rail as a LIST: everything the project holds is on it, a row
 * opens the conversation it names, and moving between two rows really moves
 * between two conversations.
 *
 * Ported by use case from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/chat/test_conversation_management.py`):
 *
 *  - `TestCreateConversation::test_create_conversation_via_api` (TC-CONV-001/003)
 *  - `TestConversationList::test_click_conversation_to_open` (TC-CONV-004)
 *  - `TestConversationActions::test_multiple_conversations_listed`
 *  - `TestConversationNavigation::test_navigate_between_conversations`
 *
 * `chat.conversation.spec.ts`'s J11 already opens a conversation by DEEP LINK,
 * which is a different thing from opening one by clicking its row: the deep
 * link exercises the route's own resolution from a cold URL, the click
 * exercises the rail's row-to-route wiring. `useConversationSidebar` keeps a
 * "restore vs click" split precisely because those two paths differ, and M1
 * in `chat.management.spec.ts` documents a defect that lived entirely in the
 * click one (a stale closure left the route on a conversation the rail had
 * already removed). So the click is asserted here on its own.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT MAKES "IT NAVIGATED" MEAN SOMETHING
 * ─────────────────────────────────────────────────────────────────────────────
 * The address alone does not. `/app/chat/:id` is a client-side route change, so
 * a rail that pushed the right URL and rendered the previous transcript would
 * satisfy any assertion made on `page.url()`. Each click below is therefore
 * measured by the request THE ROUTE issues for the conversation it is opening —
 * `GET /elitea_core/conversation/prompt_lib/{p}/{id}` — armed before the click,
 * so what is asserted is that the page went and fetched that conversation.
 */
import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import {
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createConversation,
  deleteConversation,
} from '../../fixtures/api';

/** Every entity this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-list';

const CONVERSATION_PATH = `/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}${SUFFIX}`;
}

/**
 * Opens the rail's date group that holds today's conversations.
 *
 * `DateGroup` starts collapsed, so its rows are mounted but not visible — a
 * plain `.click()` on a row would time out on "element is not visible" and read
 * like a missing row. Driven off `aria-expanded` rather than clicked
 * unconditionally, so a future default of "expanded" closes nothing.
 * (Same helper, same reasoning, as `chat.management.spec.ts`'s M1.)
 */
async function openTodayGroup(page: Page): Promise<void> {
  const sidebar = page.getByTestId('chat-conversation-sidebar');
  await expect(sidebar).toBeVisible({ timeout: 20_000 });
  const today = sidebar.getByRole('button', { name: 'Today' });
  await expect(today).toBeVisible({ timeout: 20_000 });
  if ((await today.getAttribute('aria-expanded')) !== 'true') await today.click();
}

/**
 * Clicks a rail row and asserts the route settled on that conversation.
 *
 * No network assertion: this is for a conversation the page has ALREADY opened
 * once in this test. The app's query client sets `staleTime: 30_000`
 * (`app/providers/queryClient.ts`), so a return trip inside the same half
 * minute is served from cache and issues no request at all — a wait for one
 * would hang, and a test that hangs teaches nothing about the rail.
 */
async function reopenFromRail(page: Page, conversationId: string): Promise<void> {
  await page.getByTestId(`conversation-item-${conversationId}`).click();
  await expect(page).toHaveURL(new RegExp(`/app/chat/${conversationId}$`), { timeout: 15_000 });
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 15_000 });
  await expect(page.getByText('Something went wrong.')).toHaveCount(0);
}

/** Clicks a rail row and returns only once the ROUTE has fetched that conversation. */
async function openFromRail(page: Page, conversationId: string): Promise<void> {
  const fetched = page.waitForResponse(
    (response) =>
      response.url().includes(`${CONVERSATION_PATH}/${conversationId}`) &&
      response.request().method() === 'GET',
    { timeout: 30_000 },
  );
  await page.getByTestId(`conversation-item-${conversationId}`).click();
  expect((await fetched).status(), 'the route must resolve the conversation it navigated to').toBe(200);
  // `toHaveURL`, not `waitForURL`: a history push fires no navigation
  // lifecycle event for `waitForURL` to wait on.
  await expect(page).toHaveURL(new RegExp(`/app/chat/${conversationId}$`), { timeout: 15_000 });
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 15_000 });
}

// ─────────────────────────────────────────────────────────────────────────────
// legacy: test_create_conversation_via_api + test_multiple_conversations_listed
//         + test_click_conversation_to_open
// ─────────────────────────────────────────────────────────────────────────────
test('every conversation the project holds is on the rail, and a row opens the one it names', async ({ page }) => {
  const names = [uniqueName('multi1'), uniqueName('multi2'), uniqueName('multi3')];
  const ids: string[] = [];
  for (const name of names) ids.push(await createConversation(page.request, name));

  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
  await openTodayGroup(page);

  // All three, each carrying its OWN name — the rail is keyed by conversation
  // id, so a list that rendered three rows with one name would fail here.
  for (const [index, id] of ids.entries()) {
    const row = page.getByTestId(`conversation-item-${id}`);
    await expect(row, 'a conversation created over the API must appear in the rail').toBeVisible({
      timeout: 15_000,
    });
    await expect(row).toContainText(names[index] as string);
  }

  await checkA11y(page);

  // The MIDDLE row, not the first: a rail that always opened whatever sits at
  // the top would pass on the first one.
  await openFromRail(page, ids[1] as string);
  await expect(page.getByTestId('user-message')).toHaveCount(0);

  for (const id of ids) await deleteConversation(page.request, id);
});

// ─────────────────────────────────────────────────────────────────────────────
// legacy: test_conversation_management.py::TestConversationNavigation::test_navigate_between_conversations
// ─────────────────────────────────────────────────────────────────────────────
test('the rail moves from one open conversation to another', async ({ page }) => {
  const firstName = uniqueName('nava');
  const secondName = uniqueName('navb');
  const first = await createConversation(page.request, firstName);
  const second = await createConversation(page.request, secondName);

  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
  await openTodayGroup(page);

  await openFromRail(page, first);
  await openFromRail(page, second);

  // …and back, so the move is not one-way. This is the direction that broke in
  // M1's stale-closure defect: the rail acted while the route did not.
  await reopenFromRail(page, first);

  // Both rows are still on the rail after all that — a navigation that
  // re-rendered the list from the open conversation alone would lose one.
  await expect(page.getByTestId(`conversation-item-${first}`)).toBeVisible();
  await expect(page.getByTestId(`conversation-item-${second}`)).toBeVisible();

  await deleteConversation(page.request, first);
  await deleteConversation(page.request, second);
});
