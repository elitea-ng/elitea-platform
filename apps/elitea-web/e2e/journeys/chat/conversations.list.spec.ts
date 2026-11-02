/**
 * The sidebar's Chats list (`widgets/chat-rail`): everything the project holds is on it, a row
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
 * exercises the sidebar row's row-to-route wiring. M1 in
 * `chat.management.spec.ts` documents a defect that lived entirely in the
 * click path (the route stayed on a conversation the list had already
 * removed). So the click is asserted here on its own.
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
import type { Locator, Page } from '@playwright/test';

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
const SUFFIX = '-list';

const CONVERSATION_PATH = `/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;
const FOLDER_PATH = `/elitea_core/folder/prompt_lib/${DEFAULT_PROJECT_ID}`;

/** The `?grouped=true` envelope, read for the three fields these tests assert on. */
interface GroupedListing {
  readonly date_groups?: readonly {
    readonly name?: string;
    readonly total?: number;
    readonly offset?: number;
    readonly conversations?: readonly { readonly id?: string | number }[];
  }[];
}

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}${SUFFIX}`;
}

/**
 * The sidebar caption of a date group the server named. The listing's names
 * ("Today", "Yesterday", "This week", "This month", "Older") are shown as the
 * sidebar's own captions (`widgets/chat-rail/ui/ChatsSection.tsx`'s
 * `groupCaption`).
 */
function groupCaption(serverName: string): string {
  switch (serverName.toLowerCase().replaceAll(' ', '_')) {
    case 'this_week':
      return 'Previous 7 days';
    case 'this_month':
      return 'Previous 30 days';
    default:
      return serverName;
  }
}

/** A date group's frame in the sidebar Chats list (`shell-chats-group`), by the group's server name. */
function groupFrame(page: Page, groupName: string): Locator {
  const caption = groupCaption(groupName).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  return page
    .getByTestId('shell-chats')
    .getByTestId('shell-chats-group')
    .filter({ has: page.locator('.shell-group-caption', { hasText: new RegExp(`^${caption}$`) }) });
}

/**
 * Opens the sidebar's date group the server named.
 *
 * Only Pinned and Today are open by default (and the open state is remembered
 * per viewer); a closed group renders no rows at all, so a row in it would
 * read like a missing row. Driven off `aria-expanded` rather than clicked
 * unconditionally, so an open group is never closed.
 */
async function openGroup(page: Page, groupName: string): Promise<void> {
  await expect(page.getByTestId('shell-chats')).toBeVisible({ timeout: 20_000 });
  const header = groupFrame(page, groupName).locator('.shell-row-main');
  await expect(header, `the sidebar must render the "${groupName}" group the server named`).toBeVisible({
    timeout: 20_000,
  });
  if ((await header.getAttribute('aria-expanded')) !== 'true') await header.click();
}

/** The sidebar row of a conversation; its main button opens it. */
function chatRow(page: Page, conversationId: string): Locator {
  return page.locator(`[data-conversation-id="${conversationId}"]`);
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
  await chatRow(page, conversationId).locator('.shell-row-main').click();
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
  await chatRow(page, conversationId).locator('.shell-row-main').click();
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
    const row = chatRow(page, id);
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
  await expect(chatRow(page, first)).toBeVisible();
  await expect(chatRow(page, second)).toBeVisible();

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
 * WHAT THE BUCKET'S OWN ARITHMETIC MEANS
 * ─────────────────────────────────────────────────────────────────────────────
 * The list pages a bucket: its "Show more" row fetches `date_group=…&offset=…`
 * once a bucket reports more rows than it delivered. It never could, until
 * issue 852 — the grouped listing answered with every conversation of every
 * bucket and a `total` equal to that same count, so "there is more" was not
 * expressible and the follow-up read had no caller. This test used to assert
 * exactly that: a `total` equal to the rows delivered, and no `date_group=`
 * request anywhere on the wire.
 *
 * The last test in this file is now the opposite statement, and it seeds
 * enough conversations to make it: a bucket bigger than one page reports the
 * remainder, and the rail goes and fetches it.
 */
test('the rail places each conversation in the date group the server assigned, in the server’s order', async ({
  page,
}) => {
  // The assertions below cover EVERY row the listing sent, not three of them,
  // so the budget is the page load plus one visibility check per row of every
  // bucket the rail was given — more than the default 30 s allows for on a
  // project that a whole suite has been seeding.
  test.setTimeout(120_000);
  // ONE token across all three names, so the server's own filtered listing
  // below selects exactly these three and nothing another worker made.
  const token = Math.random().toString(36).slice(2, 10);
  const names = [1, 2, 3].map((n) => `${AUTOTEST_PREFIX}when${n}-${token}${SUFFIX}`);
  const ids: string[] = [];
  for (const name of names) ids.push(await createConversation(page.request, name));

  try {
    /* ── which bucket the SERVER put these three in ─────────────────────── */
    /*
     * Asked with a `query=`, which the handler serves WHOLE — it pages only
     * an unfiltered listing. That matters since issue 852: the rail's own
     * read now carries ONE PAGE per bucket, and a date bucket is a
     * PROJECT-WIDE queue, so a concurrent journey seeding a dozen
     * conversations of its own pushes these three off page one entirely.
     * That happened, and the assertion it broke ("these three are in the
     * listing the rail read") was making a claim about paging, not about
     * grouping. The grouping claim is asked here, where it is answerable.
     */
    const filtered = await page.request.get(`${API_BASE}${FOLDER_PATH}?grouped=true&query=${encodeURIComponent(token)}`);
    expect(filtered.status(), 'the filtered grouped listing must answer').toBe(200);
    const seeded = (await filtered.json()) as GroupedListing;
    const assignedGroup = new Map<string, string>();
    for (const group of seeded.date_groups ?? []) {
      for (const conversation of group.conversations ?? []) {
        assignedGroup.set(String(conversation.id ?? ''), String(group.name ?? ''));
      }
    }
    expect(
      [...assignedGroup.keys()].sort(),
      'every conversation just created must be in a date group the server names',
    ).toEqual([...ids].sort());

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

    const listing = (await (await listed).json()) as GroupedListing;
    const groups = listing.date_groups ?? [];
    expect(groups.length, 'the grouped listing must carry the buckets the rail renders').toBeGreaterThan(0);

    /* ── what the server said, for the WHOLE page it sent ───────────────── */
    /*
     * Every row of every bucket, not just this test's own three. Which rows
     * those are is decided by whatever else the project holds at this moment,
     * so naming them would be the race again; that they are rendered, under
     * the bucket the server named and in the order the server sent, is the
     * claim this test exists to make, and it holds for any set of rows.
     *
     * Order: buckets as the response lists them (newest bucket first), rows
     * in the order inside each bucket.
     */
    const serverOrder: string[] = [];
    const groupOf = new Map<string, string>();
    for (const group of groups) {
      for (const conversation of group.conversations ?? []) {
        const id = String(conversation.id ?? '');
        serverOrder.push(id);
        groupOf.set(id, String(group.name ?? ''));
      }
    }
    expect(serverOrder.length, 'the rail’s own listing must carry rows for it to render').toBeGreaterThan(0);

    // The two listings agree about the bucket. An id the rail's page does not
    // carry is on page two of its bucket — the last test in this file fetches
    // one of those — and says nothing about grouping either way.
    for (const [id, groupName] of assignedGroup) {
      if (!groupOf.has(id)) continue;
      expect(groupOf.get(id), 'the filtered and unfiltered listings must agree about the bucket').toBe(groupName);
    }

    // Each bucket's own arithmetic is CONSISTENT: it never claims to have
    // delivered more rows than it did. A bucket bigger than one page reports
    // the remainder instead, which the last test in this file exercises.
    for (const group of groups) {
      expect(
        group.total,
        `bucket "${String(group.name)}" reports fewer rows than it delivered`,
      ).toBeGreaterThanOrEqual((group.conversations ?? []).length);
    }

    /* ── what the screen shows ──────────────────────────────────────────── */
    for (const groupName of new Set(groupOf.values())) await openGroup(page, groupName);

    for (const id of serverOrder) {
      // Visible under its own group: a CLOSED group renders no rows, and only
      // the groups the server named were opened above.
      await expect(
        groupFrame(page, String(groupOf.get(id))).locator('xpath=ancestor::li[1]').locator(`[data-conversation-id="${id}"]`),
        `the conversation must be visible under the "${String(groupOf.get(id))}" group the server assigned it`,
      ).toBeVisible({ timeout: 20_000 });
    }

    const rendered = await page
      .getByTestId('shell-chats')
      .locator('[data-conversation-id]')
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute('data-conversation-id') ?? ''));
    // Filtered to the date-group rows: the same list also renders the pinned
    // group and every folder, and the server partitions a conversation into
    // exactly one of the three, so this selects the bucket rows and nothing
    // else.
    const fromBuckets = new Set(serverOrder);
    expect(
      rendered.filter((id) => fromBuckets.has(id)),
      'the rail must keep the order the server sent — newest first, by the timestamps it grouped on',
    ).toEqual(serverOrder);
  } finally {
    for (const id of ids) await deleteConversation(page.request, id);
  }
});

// ─────────────────────────────────────────────────────────────────────────────
// issue 852: the rail pages a bucket that is bigger than one page
//
// This is the inversion of what this file used to pin. The grouped listing set
// every bucket's `total` to the number of rows it had just delivered, so the
// list's `hasMore` was false for every bucket that has ever existed: the
// next-page control never appeared, the load-more never fired, and the
// `?date_group=…&offset=…` read the server has served since the folders
// handler was fixed had no caller at all. A project longer than one page did
// not load slowly; it never loaded the rest.
//
// THE SEED IS THE POINT. The bucket has to be bigger than the server's page
// (10) for a remainder to exist, so this test creates more conversations than
// the rest of this file put together, through the API, and deletes every one
// of them afterwards.
// ─────────────────────────────────────────────────────────────────────────────
const PAGE_SIZE = 10;

test('a date bucket bigger than one page reports the remainder, and the rail fetches it', async ({ page }) => {
  // The seed and the teardown are ~24 API calls; the default 30 s budget is
  // about the clock rather than about paging.
  test.setTimeout(180_000);
  const ids: string[] = [];
  for (let i = 0; i < PAGE_SIZE + 4; i += 1) ids.push(await createConversation(page.request, uniqueName(`page${i}`)));

  try {
    // Recorded per bucket: the list pages each bucket on its own, and a
    // follow-up fired by SOME OTHER bucket says nothing about this one.
    const followUps = new Map<string, string[]>();
    page.on('request', (request) => {
      const url = new URL(request.url(), BASE_URL);
      const group = url.searchParams.get('date_group');
      if (group === null) return;
      followUps.set(group, [...(followUps.get(group) ?? []), request.url()]);
    });

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

    const listing = (await (await listed).json()) as GroupedListing;
    const groups = listing.date_groups ?? [];
    // The bucket these conversations landed in — read from the answer rather
    // than hardcoded, for the midnight reason the file header gives.
    const bucket = groups.find((group) =>
      (group.conversations ?? []).some((conversation) => ids.includes(String(conversation.id ?? ''))),
    );
    expect(bucket, 'the seeded conversations must be in a bucket the rail renders').toBeDefined();

    const delivered = (bucket?.conversations ?? []).length;
    expect(delivered, 'the bucket must be PAGED, not served whole').toBe(PAGE_SIZE);
    expect(bucket?.total ?? 0, 'the bucket must report the rows it did NOT deliver').toBeGreaterThan(delivered);
    expect(bucket?.offset, 'offset is where the next page starts — the rows delivered').toBe(delivered);

    /* ── and the rail really goes and asks for them ─────────────────────── */
    const bucketName = String(bucket?.name);
    await openGroup(page, bucketName);

    // The bucket's own subtree: its group's `li` holds its rows and its
    // "Show more" (`shell-chats-more`), and nothing of the pinned group or
    // the folders, which the same list also renders.
    const bucketBody = groupFrame(page, bucketName).locator('xpath=ancestor::li[1]');
    const bucketRowCount = (): Promise<number> => bucketBody.locator('[data-conversation-id]').count();

    const lastInBucket = String((bucket?.conversations ?? []).at(-1)?.id ?? '');
    await expect(bucketBody.locator(`[data-conversation-id="${lastInBucket}"]`)).toBeVisible({ timeout: 20_000 });
    // A bucket with a remainder ends in "Show more", which fetches its next page.
    const showMore = bucketBody.getByTestId('shell-chats-more');
    await expect(showMore, 'a bucket with a remainder must offer its next page').toBeVisible({ timeout: 20_000 });
    await showMore.locator('.shell-row-main').click();

    await expect
      .poll(() => followUps.get(bucketName)?.length ?? 0, {
        timeout: 30_000,
        message: 'a bucket with a remainder must ask the server for its next page',
      })
      .toBeGreaterThan(0);
    const followUp = new URL(followUps.get(bucketName)?.[0] ?? '', BASE_URL);
    expect(followUp.searchParams.get('offset'), 'the follow-up must start where the first page ended').toBe(
      String(bucket?.offset),
    );

    // …and the rows it fetched really reach the screen.
    await expect
      .poll(() => bucketRowCount(), {
        timeout: 30_000,
        message: 'the bucket must hold more rows than its first page carried once the second page lands',
      })
      .toBeGreaterThan(delivered);
  } finally {
    for (const id of ids) await deleteConversation(page.request, id);
  }
});
