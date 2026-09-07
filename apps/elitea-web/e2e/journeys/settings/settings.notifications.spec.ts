/**
 * Journey 31: Settings: the notification centre reads its list from the server
 * (#413).
 *
 * WHY THIS JOURNEY EXISTS
 *
 * `GET /api/v2/notifications/notifications/prompt_lib/{projectID}` answered
 * chi's default 404 on this stack. `services/elitea-main/cmd/elitea-main/
 * main.go` composed the route only inside the `authEnabled`
 * (`ELITEA_AUTH_CONFIG_FILE`) block, and this stack authenticates through the
 * OIDC session cookie instead. `internal/api/production_router.go` registers
 * the path only when `cfg.CurrentNotifications != nil`, so no path existed.
 *
 * Nothing caught it. The screen turned the 404 into "No notifications yet", so
 * the notification centre looked like a healthy, empty inbox on every clean
 * database. The resolver test for the four `models.notifications` permissions
 * still passed, because the grants were never the problem.
 *
 * The two tests below are a pair, and neither one alone is enough.
 *
 *  - J31a proves the SCREEN'S OWN request reaches a registered route. It reads
 *    the response the page issues, not one the test builds, so it fails the
 *    moment the composition regresses.
 *  - J31b proves a failed read no longer looks like an empty inbox. Without
 *    it, J31a's oracle can rot back into the same blind spot: a route that
 *    404s again would still render a clean screen.
 *
 * NO `checkA11y` HERE, deliberately. The claim under test is an HTTP status
 * and one text branch. An unrelated axe rule turning red would report a route
 * defect that does not exist.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

/**
 * The toolbar's search box, and the addressable proof that the page chrome is
 * present (`DrawerPageHeader` renders its title as a `<div>`, so the title is
 * not a role landmark).
 *
 * `Search`, EXACT — not `Search notifications…`. The screen was rebuilt to
 * match production, whose `NotificationTableToolbar.jsx:45` passes
 * `placeholder="Search"`; the longer string was this port's invention.
 * `exact` matters: the settings header's own default placeholder is
 * `Search something amazing!`, and a substring match would accept it.
 */
const searchBox = (page: import('@playwright/test').Page) =>
  page.getByPlaceholder('Search', { exact: true });

/** Every list read, for any project id — the page picks the id itself. */
const LIST_PATH_PREFIX = '/api/v2/notifications/notifications/prompt_lib/';

/** `page.route` glob for the same set of URLs. */
const LIST_GLOB = '**/api/v2/notifications/notifications/prompt_lib/**';

/**
 * Navigate into the shell, WAIT FOR THE ROUTER TO COMMIT the location, and
 * assert it left the URL alone. Same helper, and the same
 * `stripSearchParams` reason, as `settings.tokens.spec.ts` documents.
 */
async function gotoSettled(page: import('@playwright/test').Page, url: string): Promise<void> {
  await page.goto(url);
  await expect(page.getByTestId('sidebar-collapse-toggle')).toBeVisible({ timeout: 30_000 });
  await expect(page).toHaveURL(url);
}

test('J31a: the notifications screen reads its list from a registered route', async ({ page }) => {
  /*
   * Arm the listener BEFORE navigating. The page issues this request during
   * its first render, so a listener armed after `goto` can miss it and then
   * time out on a request that already succeeded.
   */
  const listRead = page.waitForResponse(
    (response) => new URL(response.url()).pathname.startsWith(LIST_PATH_PREFIX),
    { timeout: 30_000 },
  );

  await gotoSettled(page, BASE_URL + '/app/settings/notifications');

  const response = await listRead;

  /*
   * THE ASSERTION THIS JOURNEY EXISTS FOR.
   *
   * 404 means no route is registered for the path. 401 means the composed
   * AuthConfig refuses the browser session cookie — which is what setting
   * ELITEA_AUTH_CONFIG_FILE on this stack would produce, because the
   * `authEnabled` AuthConfig leaves `SessionSecret` empty. 403 means the
   * project grant is missing. Only 200 proves the whole path.
   */
  expect(
    response.status(),
    `GET ${new URL(response.url()).pathname} must answer 200. ` +
      '404 = the route is unregistered (#413), 401 = the AuthConfig refuses the ' +
      'session cookie, 403 = the project grant is missing.',
  ).toBe(200);

  // A 200 with the wrong body would still be a broken screen. The list
  // contract is `{total, rows}` (internal/api/v2/notifications/api.go).
  const body = (await response.json()) as { total?: unknown; rows?: unknown };
  expect(typeof body.total).toBe('number');
  expect(Array.isArray(body.rows)).toBe(true);

  // The screen itself is alive and reports no failure.
  await expect(searchBox(page)).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole('alert')).toHaveCount(0);
});

test('J31b: a failing notifications read shows an error, not an empty inbox', async ({ page }) => {
  /*
   * Reproduce the exact response the unregistered route gave: chi's default
   * NotFound handler, which answers `404 page not found` as `text/plain`.
   */
  await page.route(LIST_GLOB, (route) =>
    route.fulfill({ status: 404, contentType: 'text/plain', body: '404 page not found' }),
  );

  await gotoSettled(page, BASE_URL + '/app/settings/notifications');

  // The copy `shared/lib/http-error.ts` builds for a 404.
  await expect(page.getByRole('alert')).toHaveText('The requested resource was not found!', {
    timeout: 20_000,
  });

  // "not an empty inbox" — the branch that hid #413 must not render.
  await expect(page.getByText('No notifications yet', { exact: true })).toHaveCount(0);

  // The failure is scoped to the list body. The rest of the screen still works.
  await expect(searchBox(page)).toBeVisible();
});
