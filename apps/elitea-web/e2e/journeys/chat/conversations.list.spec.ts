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
async function openGroup(page: Page, groupName: string): Promise<void> {
  const sidebar = page.getByTestId('chat-conversation-sidebar');
  await expect(sidebar).toBeVisible({ timeout: 20_000 });
  const header = sidebar.getByRole('button', { name: groupName, exact: true });
  await expect(header, `the rail must render the "${groupName}" group the server named`).toBeVisible({
    timeout: 20_000,
  });
  if ((await header.getAttribute('aria-expanded')) !== 'true') await header.click();
}

/** The group every conversation created during a run belongs to. */
async function openTodayGroup(page: Page): Promise<void> {
  await openGroup(page, 'Today');
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

/*
 * The rail as a TIMELINE: which date bucket each conversation lands in, the
 * order inside it, and how much of it arrives at once.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE EXPECTED GROUP IS TAKEN FROM THE PAGE'S OWN RESPONSE
 * ─────────────────────────────────────────────────────────────────────────────
 * The buckets are computed SERVER-side (`groupByDate`, against the server's own
 * clock) and the label travels on the wire; the rail only renders what it was
 * told. A journey that hardcoded "Today" would therefore be asserting the two
 * machines agree about the date — which is a real, recurring failure here
 * (a run that crosses midnight has already broken a seeded assertion once) and
 * is not what this rail is for. So the expectation is read out of THE VERY
 * RESPONSE THE PAGE FETCHED, and what is asserted is that the screen agrees
 * with it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THE "NO SECOND PAGE" ASSERTIONS MEAN
 * ─────────────────────────────────────────────────────────────────────────────
 * The rail CAN page a bucket — `LoadMoreSentinel` fires `date_group=…&offset=…`
 * once a bucket reports more rows than it delivered — and today it never does,
 * because this server answers the grouped listing with every conversation of
 * every bucket and a `total` equal to that same count. Both halves are
 * asserted: the answer's own arithmetic, and the absence of any follow-up page
 * request on the wire.
 *
 * That is the load-bearing precondition for the two tests above, which find
 * their rows without ever scrolling the rail. The day the server starts paging
 * a bucket, this test fails and names the reason — instead of those two
 * starting to lose rows in a way that reads like a missing conversation.
 */
test('the rail places each conversation in the date group the server assigned, in the server’s order', async ({
  page,
}) => {
  const names = [uniqueName('when1'), uniqueName('when2'), uniqueName('when3')];
  const ids: string[] = [];
  for (const name of names) ids.push(await createConversation(page.request, name));

  try {
    // Every request the page makes from here, so the "no second page" claim
    // below is measured on the wire rather than inferred from the screen.
    const requested: string[] = [];
    page.on('request', (request) => requested.push(request.url()));

    // Armed before the navigation: this is the rail's OWN read, and the
    // load-more fetchers hit the same path with a `date_group`/`folder_id`
    // parameter, so both are excluded here.
    const listed = page.waitForResponse(
      (response) =>
        response.request().method() === 'GET' &&
        response.url().includes('/elitea_core/folder/prompt_lib/') &&
        response.url().includes('grouped=true') &&
        !response.url().includes('date_group=') &&
        !response.url().includes('folder_id='),
      { timeout: 30_000 },
    );
    await page.goto(BASE_URL + '/app/chat');
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

    const listing = (await (await listed).json()) as {
      date_groups?: readonly {
        readonly name?: string;
        readonly total?: number;
        readonly conversations?: readonly { readonly id?: string | number }[];
      }[];
    };
    const groups = listing.date_groups ?? [];
    expect(groups.length, 'the grouped listing must carry the buckets the rail renders').toBeGreaterThan(0);

    /* ── what the server said about THESE three ─────────────────────────── */
    // The flattened server order: buckets in the order the response lists them
    // (newest bucket first), rows in the order inside each bucket.
    const serverOrder: string[] = [];
    const groupOf = new Map<string, string>();
    for (const group of groups) {
      for (const conversation of group.conversations ?? []) {
        const id = String(conversation.id ?? '');
        if (!ids.includes(id)) continue;
        serverOrder.push(id);
        groupOf.set(id, String(group.name ?? ''));
      }
    }
    expect(
      [...serverOrder].sort(),
      'every conversation just created must be in the grouped listing the rail reads',
    ).toEqual([...ids].sort());

    // Every bucket arrives WHOLE: `total` is the count of rows delivered, so
    // there is nothing left for a second page to fetch.
    for (const group of groups) {
      expect(
        group.total,
        `bucket "${String(group.name)}" reports a total that does not match the rows it carries`,
      ).toBe((group.conversations ?? []).length);
    }

    /* ── what the screen shows ──────────────────────────────────────────── */
    for (const groupName of new Set(groupOf.values())) await openGroup(page, groupName);

    for (const id of ids) {
      // Visible, not merely attached: a row sitting in a COLLAPSED bucket is
      // mounted but hidden, so visibility here is what says the row is under
      // the header the server named.
      await expect(
        page.getByTestId(`conversation-item-${id}`),
        `the conversation must be visible under the "${String(groupOf.get(id))}" group the server assigned it`,
      ).toBeVisible({ timeout: 20_000 });
    }

    const rendered = await page
      .getByTestId('chat-conversation-sidebar')
      .locator('[data-testid^="conversation-item-"]')
      .evaluateAll((nodes) =>
        nodes.map((node) => (node.getAttribute('data-testid') ?? '').replace('conversation-item-', '')),
      );
    expect(
      rendered.filter((id) => ids.includes(id)),
      'the rail must keep the order the server sent — newest first, by the timestamps it grouped on',
    ).toEqual(serverOrder);

    /* ── and nothing on this rail asked for a second page ───────────────── */
    // Not read as a missing sentinel element: a folder accordion renders one of
    // those too, and a folder that holds a PINNED conversation is short by
    // client-side filtering rather than by paging — so a count taken at page
    // scope would flake on another journey's fixtures. The REQUEST is the
    // unambiguous fact.
    expect(
      requested.filter((url) => url.includes('date_group=')),
      'no bucket reported more rows than it delivered, so nothing had a second page to ask for',
    ).toEqual([]);
  } finally {
    for (const id of ids) await deleteConversation(page.request, id);
  }
});
