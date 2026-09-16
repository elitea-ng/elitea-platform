/**
 * The bell popover's infinite scroll (issue 940/A4, ELITEA-0747/ELITEA-0748).
 *
 * `widgets/sidebar/ui/NotificationButton.tsx` used to show a fixed top-5
 * unread rows via a single `useNotificationsList` call, with no way to see
 * anything past it — `settings.notification-center.spec.ts`'s own header
 * used to record the two cases below as not-applicable for exactly that
 * reason. The popover now pages through the same list route on scroll
 * (`useNotificationsInfiniteList`), 20 rows per group — the settings page's
 * own page-based pagination (`NotificationsTablePagination`) is a separate,
 * untouched consumer of the same route.
 *
 * WHY THE LIST IS MOCKED: same reason `settings.notification-center.spec.ts`
 * gives — no route creates a real notification, so 45 real unread rows
 * cannot be seeded through the product. The mock here, unlike that file's
 * own (which ignores `offset`/`limit` and returns everything), DOES respect
 * them: it is the one thing this test is actually about.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

const LIST_PATH_RE = /\/api\/v2\/notifications\/notifications\/prompt_lib\/\d+/;

interface MockRow {
  readonly id: number;
  readonly event_type: string;
  readonly created_at: string;
  readonly is_seen: boolean;
  readonly meta: { readonly message: string };
}

function makeRows(count: number, startId: number): MockRow[] {
  return Array.from({ length: count }, (_, i) => ({
    id: startId + i,
    event_type: 'generic',
    created_at: new Date(Date.now() - i * 1000).toISOString(),
    is_seen: false,
    meta: { message: `autotest_ scroll row ${startId + i}` },
  }));
}

/**
 * Serves the LIST GET paged by the REAL `limit`/`offset` query params the
 * popover's infinite query sends, and records every offset requested so a
 * test can assert exactly how many groups were fetched.
 */
async function mockPagedNotifications(page: Page, allRows: readonly MockRow[]): Promise<{ readonly offsetsRequested: number[] }> {
  const offsetsRequested: number[] = [];
  await page.route(LIST_PATH_RE, async (route) => {
    if (route.request().method() !== 'GET') {
      await route.continue();
      return;
    }
    const url = new URL(route.request().url());
    const onlyNew = url.searchParams.get('only_new') === 'true';
    const limitParam = url.searchParams.get('limit');
    const offsetParam = url.searchParams.get('offset');
    const source = onlyNew ? allRows.filter((r) => !r.is_seen) : allRows;

    // The bell's own unread-badge query asks `limit=1`; only the popover's
    // own `limit=20` group requests are what this test is about.
    if (limitParam === '20') offsetsRequested.push(Number(offsetParam ?? '0'));

    const limit = Number(limitParam ?? '20');
    const offset = Number(offsetParam ?? '0');
    const rows = source.slice(offset, offset + limit);
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ total: source.length, rows }) });
  });
  return { offsetsRequested };
}

/**
 * `NotificationButton`'s click handler branches on `personalProjectId`
 * (route-context `auth`, resolved asynchronously): while it is still
 * unresolved, a click NAVIGATES to `/chat` (the graceful "no personal
 * project yet" fallback) instead of opening the popover — silently, with
 * no error and no visible change if already on `/chat`. Waiting for the
 * unread-dot (gated on the SAME `personalProjectId`, via the badge query's
 * `enabled: !!personalProjectId`) is what `settings.notification-center.
 * spec.ts`'s own ELITEA-0742 test already synchronizes on before its first
 * click, for the identical reason — reproduced here, since without it this
 * suite's very first click raced that resolution under parallel load
 * (measured: ~3/8 runs).
 */
async function openChatWithBell(page: Page): Promise<void> {
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('sidebar-notification-button')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('sidebar-notification-unread-dot')).toBeVisible({ timeout: 15_000 });
}

async function openPopover(page: Page): Promise<void> {
  await page.getByTestId('sidebar-notification-button').click();
  await expect(page.getByTestId('sidebar-notification-scroll-area')).toBeVisible({ timeout: 15_000 });
}

async function scrollToBottom(page: Page): Promise<void> {
  const area = page.getByTestId('sidebar-notification-scroll-area');
  await area.evaluate((el) => {
    el.scrollTop = el.scrollHeight;
    el.dispatchEvent(new Event('scroll'));
  });
}

/* onetest: ELITEA-0748 — infinite scroll appends new groups below the ones already loaded; the last,
 * partial group (< 20 items) is displayed correctly and does not auto-trigger another load. */
test('ELITEA-0748: scrolling the popover appends the next group without replacing the first, and stops after the last partial group', async ({ page }) => {
  test.setTimeout(60_000);
  // 45 rows: a full first group (20), a full second group (20), a partial
  // third group (5) — the one that must stop the scroll loader for good.
  const rows = makeRows(45, 5_000);
  const { offsetsRequested } = await mockPagedNotifications(page, rows);

  await openChatWithBell(page);
  await openPopover(page);

  await expect(page.getByText('autotest_ scroll row 5000')).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText('autotest_ scroll row 5019')).toBeVisible();
  // Group 2 has not loaded yet — its first row must be absent.
  await expect(page.getByText('autotest_ scroll row 5020')).toHaveCount(0);

  await scrollToBottom(page);
  await expect(page.getByText('autotest_ scroll row 5020')).toBeVisible({ timeout: 15_000 });
  // Group 1's rows are still on screen — appended, not replaced.
  await expect(page.getByText('autotest_ scroll row 5000')).toBeVisible();

  await scrollToBottom(page);
  await expect(page.getByText('autotest_ scroll row 5044')).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText('autotest_ scroll row 5000')).toBeVisible();
  await expect(page.getByText('No new notifications right now')).toHaveCount(0);

  // One more scroll past the fully-loaded list must not fire a fourth
  // request — `hasNextPage` is false once every row is accounted for.
  expect(offsetsRequested).toEqual([0, 20, 40]);
  await scrollToBottom(page);
  await page.waitForTimeout(300);
  expect(offsetsRequested, 'a scroll past the fully-loaded list must not fire another request').toEqual([0, 20, 40]);
});

/* onetest: ELITEA-0747 — closing and reopening the popover fully resets the scroll load state: it
 * starts from the first group again, and scrolling from there still loads correctly, across more
 * than one close/reopen cycle. */
test('ELITEA-0747: scroll load state resets on popup close and reopen, repeatedly', async ({ page }) => {
  test.setTimeout(60_000);
  const rows = makeRows(45, 6_000);
  await mockPagedNotifications(page, rows);

  await openChatWithBell(page);
  await openPopover(page);
  await expect(page.getByText('autotest_ scroll row 6000')).toBeVisible({ timeout: 15_000 });

  await scrollToBottom(page);
  await expect(page.getByText('autotest_ scroll row 6020')).toBeVisible({ timeout: 15_000 });

  // Close: the popover unmounts (`NotificationButton.tsx` gates the
  // `Popover` on `anchorEl`, not a mere `open` toggle) and the infinite
  // query's cache for this project is dropped.
  await page.getByRole('button', { name: 'Close notifications' }).click();
  await expect(page.getByTestId('sidebar-notification-scroll-area')).toHaveCount(0, { timeout: 10_000 });

  // Reopen: group 1 only, exactly like the very first open — no stale
  // accumulated 45-row state from the previous session.
  await openPopover(page);
  await expect(page.getByText('autotest_ scroll row 6000')).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText('autotest_ scroll row 6020')).toHaveCount(0);

  // And scrolling still loads correctly post-reopen.
  await scrollToBottom(page);
  await expect(page.getByText('autotest_ scroll row 6020')).toBeVisible({ timeout: 15_000 });

  // A second close/reopen cycle behaves identically.
  await page.getByRole('button', { name: 'Close notifications' }).click();
  await expect(page.getByTestId('sidebar-notification-scroll-area')).toHaveCount(0, { timeout: 10_000 });
  await openPopover(page);
  await expect(page.getByText('autotest_ scroll row 6000')).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText('autotest_ scroll row 6020')).toHaveCount(0);
});
