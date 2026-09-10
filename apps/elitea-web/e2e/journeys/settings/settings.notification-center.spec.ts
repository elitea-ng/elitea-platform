/**
 * The notification centre's mark-as-read / mark-as-unread actions — the
 * settings table's bulk toggle, and the bell popover's "Mark all as read".
 *
 * Ported by use case from the w1-notifications package (ELITEA-0741,
 * ELITEA-0742, ELITEA-0745). `settings.notifications.spec.ts` (J31a/J31b)
 * already proves the screen reads its list from a registered route and
 * reports a failed read honestly; this file is the mutation half.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE LIST IS MOCKED
 * ─────────────────────────────────────────────────────────────────────────────
 * There is no route that CREATES a notification (`internal/api/v2/
 * notifications/api.go` serves list/details/mark-seen/delete only — every
 * real row comes from a product event: a bucket-retention sweep, a failed
 * scheduled reindex). Rather than wait on one of those (or skip the use
 * case), the LIST read (`GET /notifications/notifications/prompt_lib/
 * {projectId}`) is intercepted with `page.route`, exactly as
 * `settings.notifications.spec.ts`'s own J31b intercepts the same path to
 * prove the OTHER half of this screen. The mutation route
 * (`PUT` on the same path) is real product code reached through the real
 * button: this file asserts the exact request body the mutation sends,
 * which is the one thing a mock list cannot fake for itself.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT IS NOT PORTED HERE (see ledger + not-applicable.md)
 * ─────────────────────────────────────────────────────────────────────────────
 * ELITEA-0747/ELITEA-0748 (scroll-load state resets on popup close/reopen;
 * infinite-scroll groups) describe a popover this app does not have: the
 * real bell popover (`widgets/sidebar/ui/NotificationButton.tsx`) shows a
 * FIXED top-5 unread rows with no scroll handler at all, and the full list
 * lives on this settings page with ordinary page-based pagination
 * (`NotificationsTablePagination`), not infinite scroll. Recorded NA.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

// No trailing `$`: the real list read carries a query string
// (page/pageSize/sort/search), and the badge/popover reads carry their own
// (`only_new`, `page_size`) — this must match regardless of what follows the
// project id.
const LIST_PATH_RE = /\/api\/v2\/notifications\/notifications\/prompt_lib\/\d+/;

interface MockRow {
  id: number;
  event_type: string;
  created_at: string;
  is_seen: boolean;
  meta: { message: string };
}

/**
 * Serves the LIST GET from mutable in-memory rows, and records every write
 * this test makes — a stateful mock, so a mark-toggle's own refetch shows
 * the row the mutation just changed, the way the real server would.
 */
function stateOfRows(initial: readonly MockRow[]): {
  readonly rows: MockRow[];
  readonly writes: Array<{ ids: readonly string[] | 'all'; is_seen: boolean }>;
} {
  return { rows: initial.map((r) => ({ ...r })), writes: [] };
}

async function mockNotifications(
  page: Page,
  state: ReturnType<typeof stateOfRows>,
): Promise<void> {
  await page.route(LIST_PATH_RE, async (route) => {
    const method = route.request().method();
    if (method === 'GET') {
      // The badge query and the popover both ask `only_new=true` — the same
      // route the full settings table reads with no filter at all, so this
      // mock must honour the query the same way the real server does, or
      // the badge/dot never reflects a mark-all-as-read the way ELITEA-0742
      // is about.
      const onlyNew = new URL(route.request().url()).searchParams.get('only_new') === 'true';
      const rows = onlyNew ? state.rows.filter((r) => !r.is_seen) : state.rows;
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ total: rows.length, rows }),
      });
      return;
    }
    if (method === 'PUT') {
      const payload = route.request().postDataJSON() as { ids: readonly string[] | 'all'; is_seen: boolean };
      state.writes.push(payload);
      if (payload.ids === 'all') {
        state.rows.forEach((r) => {
          r.is_seen = payload.is_seen;
        });
      } else {
        state.rows.forEach((r) => {
          // Ids leave the client as STRINGS — `normalizeNotification`
          // (`api/normalize.ts`) coerces every id with `String(wire.id)`,
          // and the bulk mutation round-trips whatever id the row carries
          // client-side, not the wire's original numeric one.
          if (payload.ids.includes(String(r.id))) r.is_seen = payload.is_seen;
        });
      }
      await route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
      return;
    }
    await route.continue();
  });
}

async function gotoSettled(page: Page): Promise<void> {
  await page.goto(BASE_URL + '/app/settings/notifications');
  await expect(page.getByTestId('sidebar-collapse-toggle')).toBeVisible({ timeout: 30_000 });
}

/* ── ELITEA-0745: mark a single UNREAD row as read ───────────────────────── */

test('ELITEA-0745: selecting an unread row and marking it read sends the right write and updates the row', async ({
  page,
}) => {
  const state = stateOfRows([
    { id: 9001, event_type: 'generic', created_at: new Date().toISOString(), is_seen: false, meta: { message: 'autotest_ read-me notification' } },
    { id: 9002, event_type: 'generic', created_at: new Date().toISOString(), is_seen: true, meta: { message: 'autotest_ already-read notification' } },
  ]);
  await mockNotifications(page, state);
  await gotoSettled(page);

  await expect(page.getByTestId('notifications-table-row')).toHaveCount(2, { timeout: 20_000 });
  await page.getByTestId('notification-checkbox-9001').click();

  const markRead = page.getByRole('button', { name: 'Mark selected as read' });
  await expect(markRead).toBeVisible();
  await markRead.click();

  await expect(page.getByRole('alert')).toHaveText('Notifications marked as read');
  expect(state.writes).toEqual([{ ids: ['9001'], is_seen: true }]);
  expect(state.rows.find((r) => r.id === 9001)?.is_seen).toBe(true);

  // The selection is cleared on success — both bulk actions disable again.
  await expect(page.getByRole('button', { name: /^Mark selected as/ })).toBeDisabled();
});

/* ── ELITEA-0741: mark a READ row back to unread ─────────────────────────── */

test('ELITEA-0741: selecting an already-read row and marking it unread reverts it', async ({ page }) => {
  const state = stateOfRows([
    { id: 9101, event_type: 'generic', created_at: new Date().toISOString(), is_seen: true, meta: { message: 'autotest_ revert-me notification' } },
  ]);
  await mockNotifications(page, state);
  await gotoSettled(page);

  await expect(page.getByTestId('notifications-table-row')).toHaveCount(1, { timeout: 20_000 });
  await page.getByTestId('notification-checkbox-9101').click();

  // The toggle reads the SELECTION, not a fixed label: a selection that is
  // entirely already-read offers "Mark selected as unread", the reverse of
  // the previous test's all-unread selection (`hasUnreadAmongSelected`).
  const markUnread = page.getByRole('button', { name: 'Mark selected as unread' });
  await expect(markUnread).toBeVisible();
  await markUnread.click();

  await expect(page.getByRole('alert')).toHaveText('Notifications marked as unread');
  expect(state.writes).toEqual([{ ids: ['9101'], is_seen: false }]);
  expect(state.rows.find((r) => r.id === 9101)?.is_seen).toBe(false);
});

/* ── ELITEA-0742: "Mark all as read" from the bell popover reaches every unread row, not just the ones on screen ── */

test('ELITEA-0742: the bell popover’s Mark all as read sends ids: "all", not the visible page', async ({
  page,
}) => {
  const state = stateOfRows([
    { id: 9201, event_type: 'generic', created_at: new Date().toISOString(), is_seen: false, meta: { message: 'autotest_ popover row one' } },
    { id: 9202, event_type: 'generic', created_at: new Date().toISOString(), is_seen: false, meta: { message: 'autotest_ popover row two' } },
  ]);
  await mockNotifications(page, state);

  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('sidebar-notification-button')).toBeVisible({ timeout: 30_000 });
  // The unread dot must be showing before this proves anything about a
  // "mark all as read" that actually had unread rows to clear.
  await expect(page.getByTestId('sidebar-notification-unread-dot')).toBeVisible({ timeout: 15_000 });

  await page.getByTestId('sidebar-notification-button').click();
  await expect(page.getByText('autotest_ popover row one')).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText('autotest_ popover row two')).toBeVisible();

  await page.getByRole('button', { name: 'Mark all as read' }).click();

  await expect
    .poll(() => state.writes, { message: 'the popover must send ids: "all", not the two visible row ids' })
    .toEqual([{ ids: 'all', is_seen: true }]);

  // The dot clears once the badge query re-reads a dataset with no unread
  // rows left.
  await expect(page.getByTestId('sidebar-notification-unread-dot')).toHaveCount(0, { timeout: 15_000 });
});
