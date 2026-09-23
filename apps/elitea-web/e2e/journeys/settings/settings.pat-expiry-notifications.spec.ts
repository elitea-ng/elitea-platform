/**
 * Personal-access-token expiry notices (issue #940 A3 — ELITEA-0750, 0751,
 * 0752, 0754, 0755, 0756).
 *
 * ## What is under test, and where the trigger comes from
 *
 * The producer is a periodic job on the platform scheduler
 * (`internal/runtimecomposition/pat_expiry_sweep.go`), the same plane the
 * bucket-retention notices run on. A journey cannot wait for a cadence, so it
 * drives the SAME notifier through the operator route that exists for exactly
 * this reason — `POST /admin/background_jobs/administration/
 * pat_expiry_notices:run`, on the Tasks surface, behind the same
 * `runtime.plugins` gate that surface already carries. It is not a test-only
 * back door: an operator diagnosing "my users were never warned" has no other
 * way to ask.
 *
 * ## Why every token here has a 25-hour life
 *
 * A key minted through the API has the same "time left" as "total lifetime",
 * and ELITEA-0755 forbids warning about a key whose whole life is 24 hours or
 * less. So nothing created inside a test is ever eligible under the PRODUCTION
 * 24-hour window — the eligible token below lives 25 hours and is reached with
 * a 26-hour look-ahead, which is also what makes the window assertion real:
 * the default run finds it and produces nothing.
 *
 * The 26 hours is deliberately narrow for a second reason. The pass is
 * deployment-wide, and this stack is shared with every other journey running
 * at the same time: a 60-day look-ahead would announce every 30-day key
 * `settings.tokens.spec.ts` mints and fill other people's inboxes. A 26-hour
 * window reaches this file's own tokens and essentially nothing else.
 *
 * ## The UI half
 *
 * `personal_access_token_expiring` was ALREADY rendered — `features/
 * notifications/lib/routes.ts` resolves it to `/settings/tokens` and
 * `legacyText.ts` carries its copy. The producer was the missing half, so the
 * UI assertion below is a wiring check (the row appears, its link points at
 * Settings › Tokens) rather than a port of new rendering.
 */
import { test, expect, request as apiRequest } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import { acquireNamedLock } from '../../fixtures/mutex';

/** The event type the producer writes and `routes.ts` resolves. */
const EVENT_TYPE = 'personal_access_token_expiring';

/**
 * Unique per run, per file AND per TEST.
 *
 * `Date.now()` alone is not: two `--repeat-each` copies of one test start in
 * the same millisecond, mint tokens with the SAME name, and then each find two
 * notices where they assert one. The random suffix is what makes
 * `expiryNoticesFor(name)` a question about this test's own token.
 */
const stamp = (): string => `${Date.now()}-${Math.random().toString(36).slice(2, 8)}-pat`;

interface CreatedToken {
  readonly uuid: string;
  readonly name: string;
}

interface NotificationRow {
  readonly event_type?: string;
  readonly meta?: { readonly token_name?: string; readonly message?: string };
}

/**
 * Mints one token for the CALLING persona.
 *
 * `hours` rather than `days`: see the file header — the whole-lifetime rule is
 * measured in hours and a 30-day key cannot exercise either side of it.
 */
async function mintToken(api: APIRequestContext, name: string, hours: number): Promise<CreatedToken> {
  const response = await api.post(`${API_BASE}/auth/token/`, {
    data: { name, expires: { measure: 'hours', value: hours } },
  });
  expect(response.status(), `mint token ${name}: ${await response.text()}`).toBe(200);
  const body = (await response.json()) as CreatedToken;
  expect(body.uuid).toMatch(/^[0-9a-f-]{36}$/i);
  return body;
}

async function deleteToken(api: APIRequestContext, uuid: string): Promise<void> {
  await api.delete(`${API_BASE}/auth/token/${uuid}`);
}

/**
 * Runs one pass as ADMIN. The route is gated on `runtime.plugins` in
 * administration mode, which the member persona does not hold — a member
 * cannot trigger a deployment-wide sweep, and that is the correct shape.
 */
async function runExpiryPass(within: string): Promise<{ examined: number; produced: number; skipped: number }> {
  const admin = await apiRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.admin });
  try {
    const response = await admin.post(
      `${API_BASE}/admin/background_jobs/administration/pat_expiry_notices:run?within=${within}`,
    );
    expect(response.status(), `run the expiry pass: ${await response.text()}`).toBe(200);
    return (await response.json()) as { examined: number; produced: number; skipped: number };
  } finally {
    await admin.dispose();
  }
}

/** The caller's own notification list. It is user-scoped server-side — the reader filters on `user_id` alone. */
async function readNotifications(api: APIRequestContext): Promise<NotificationRow[]> {
  const response = await api.get(
    `${API_BASE}/notifications/notifications/prompt_lib/${DEFAULT_PROJECT_ID}?limit=200`,
  );
  expect(response.status(), `read notifications: ${await response.text()}`).toBe(200);
  const body = (await response.json()) as { rows?: NotificationRow[] };
  return body.rows ?? [];
}

function expiryNoticesFor(rows: readonly NotificationRow[], tokenName: string): NotificationRow[] {
  return rows.filter((row) => row.event_type === EVENT_TYPE && row.meta?.token_name === tokenName);
}

test.describe('personal access token expiry notices', () => {
  /*
   * ONE TEST AT A TIME, ACROSS WORKERS.
   *
   * The pass is DEPLOYMENT-WIDE: it has no "only this token" form, and nor
   * should it — an operator asking "who is about to lose a key" is asking
   * about the deployment. Under `fullyParallel` two tests ran together and one
   * test's 26-hour pass announced the other's key while that test was still
   * asserting a 24-hour window had NOT announced it. That is a clobber, and it
   * reads exactly like the window predicate being broken.
   *
   * `test.describe.configure({ mode: 'serial' })` is not enough on its own:
   * `--repeat-each` copies form separate serial groups and still land on
   * different workers at the same time. The mutex is what actually excludes
   * them, and it is the same mechanism the project-context journeys take
   * (`fixtures/mutex.ts`).
   */
  let releaseSweep: (() => Promise<void>) | undefined;

  test.beforeEach(async () => {
    releaseSweep = await acquireNamedLock('pat-expiry-sweep');
  });

  test.afterEach(async () => {
    const release = releaseSweep;
    releaseSweep = undefined;
    await release?.();
  });

  /* ── PATX1 ──────────────────────────────────────────────────────────────
   * onetest: ELITEA-0750, ELITEA-0752, ELITEA-0754, ELITEA-0755, ELITEA-0756
   *
   *   0752 — the notice is produced for a key inside the warning window, and
   *          NOT for the same key while it is outside it (the default run
   *          below finds nothing).
   *   0755 — a key whose whole life is 24 hours or less is never announced,
   *          even though it sits inside every window.
   *   0750 — the notice names the token and says what happens.
   *   0754 — a second pass produces nothing; the mark is not the
   *          notification row, so deleting the row would not resurrect it.
   *   0756 — the owner gets it; the other persona does not.
   * ──────────────────────────────────────────────────────────────────── */
  test('PATX1: an expiring token warns its owner once, and nobody else', async ({ page, request }) => {
    const eligibleName = `${AUTOTEST_PREFIX}patx_eligible_${stamp()}`;
    const shortLivedName = `${AUTOTEST_PREFIX}patx_short_${stamp()}`;
    let eligible: CreatedToken | undefined;
    let shortLived: CreatedToken | undefined;

    try {
      // 25 hours: past the whole-lifetime floor, and one hour outside the
      // production window.
      eligible = await mintToken(request, eligibleName, 25);
      // 12 hours: inside every window, and under the floor.
      shortLived = await mintToken(request, shortLivedName, 12);

      // ELITEA-0752, the negative half: under the SHIPPED 24-hour window the
      // 25-hour key is not yet due, so nothing is produced for it.
      await runExpiryPass('24h');
      expect(
        expiryNoticesFor(await readNotifications(request), eligibleName),
        'a key 25 hours from expiry is outside the 24-hour warning window',
      ).toHaveLength(0);

      // …and one hour of look-ahead later, it is.
      const first = await runExpiryPass('26h');
      expect(first.produced).toBeGreaterThan(0);

      const afterFirst = await readNotifications(request);
      const notices = expiryNoticesFor(afterFirst, eligibleName);
      expect(notices, 'the owner was not warned about a key inside the window').toHaveLength(1);

      // ELITEA-0750: the message names the token and says what happens to it.
      const message = notices[0]?.meta?.message ?? '';
      expect(message).toContain(eligibleName);
      expect(message).toMatch(/expire/i);
      // ELITEA-0751's data half: the action link is an EMPTY-href `[text]()`
      // segment, which the client resolves to /settings/tokens. A real URL
      // here would render as literal text instead of a link.
      expect(message).toContain('[Manage Personal Access Tokens]()');

      // ELITEA-0755: the 12-hour key is inside the 26-hour window and was
      // still not announced.
      expect(
        expiryNoticesFor(afterFirst, shortLivedName),
        'a key whose whole life is under a day must not be warned about',
      ).toHaveLength(0);

      // ELITEA-0754: a second pass adds nothing.
      await runExpiryPass('26h');
      expect(expiryNoticesFor(await readNotifications(request), eligibleName)).toHaveLength(1);

      // ELITEA-0756: the other persona's inbox never carries this token.
      const other = await apiRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.admin });
      try {
        expect(
          expiryNoticesFor(await readNotifications(other), eligibleName),
          "one user's key expiry reached another user's inbox",
        ).toHaveLength(0);
      } finally {
        await other.dispose();
      }

      /* ── PATX1's UI half (ELITEA-0751) ─────────────────────────────────
       * The row renders on Settings › Notifications and its action link
       * points at Settings › Tokens — the resolver `routes.ts` already had,
       * now with a producer feeding it.
       * ──────────────────────────────────────────────────────────────── */
      await page.goto(`${BASE_URL}/app/settings/notifications`, { waitUntil: 'domcontentloaded' });
      await expect(page.getByTestId('sidebar-collapse-toggle')).toBeVisible({ timeout: 30_000 });
      const row = page.getByText(eligibleName, { exact: false }).first();
      await expect(row).toBeVisible({ timeout: 20_000 });
      const link = page.getByRole('link', { name: 'Manage Personal Access Tokens' }).first();
      await expect(link).toBeVisible({ timeout: 10_000 });
      await expect(link).toHaveAttribute('href', /\/settings\/tokens$/);
    } finally {
      if (eligible !== undefined) await deleteToken(request, eligible.uuid);
      if (shortLived !== undefined) await deleteToken(request, shortLived.uuid);
    }
  });

  /* ── PATX2 ──────────────────────────────────────────────────────────────
   * onetest: ELITEA-0751 — following the action link lands on the tokens
   * page. Split from PATX1 so a rendering regression and a NAVIGATION
   * regression fail separately; the link's href is asserted above, this is
   * the click.
   * ──────────────────────────────────────────────────────────────────── */
  test('PATX2: the notice\'s action link opens Settings › Tokens', async ({ page, request }) => {
    const name = `${AUTOTEST_PREFIX}patx_link_${stamp()}`;
    let token: CreatedToken | undefined;
    try {
      token = await mintToken(request, name, 25);
      await runExpiryPass('26h');
      expect(expiryNoticesFor(await readNotifications(request), name)).toHaveLength(1);

      await page.goto(`${BASE_URL}/app/settings/notifications`, { waitUntil: 'domcontentloaded' });
      await expect(page.getByText(name, { exact: false }).first()).toBeVisible({ timeout: 30_000 });

      // The link opens in a new tab (`isNewTab: true`, LegacyNotificationMessage's
      // own contract for this event type), so the destination is the popup's
      // URL and not this page's.
      const opened = page.context().waitForEvent('page');
      await page.getByRole('link', { name: 'Manage Personal Access Tokens' }).first().click();
      const tokensTab = await opened;
      await tokensTab.waitForLoadState('domcontentloaded');
      expect(tokensTab.url()).toContain('/settings/tokens');
      await tokensTab.close();
    } finally {
      if (token !== undefined) await deleteToken(request, token.uuid);
    }
  });
});
