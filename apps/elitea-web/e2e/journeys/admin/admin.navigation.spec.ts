/**
 * Journey 37: An operator can REACH every admin page by clicking (JRNY-037)
 *
 * ## Why this journey exists, and why the suite was green without it
 *
 * Unit A14 ported ten admin pages and #223 retired `apps/admin-ui`, making this
 * SPA the one that ships. Its root route was a bare `<Outlet/>`: every page
 * worked by URL and NOTHING in the product linked any of them. An operator
 * opening `/admin/app/` landed on Users and had no way to reach the other nine.
 *
 * The whole admin suite was green throughout, because every journey in this
 * directory reaches its page with `page.goto(BASE_URL + '/admin/app/<path>')`.
 * `goto` is a deep link; it proves a route resolves and says nothing about
 * whether a human could get there. A suite built entirely out of deep links
 * cannot see a missing navigation — that is issue #225's actual finding, and it
 * is a property of the tests, not of the product.
 *
 * So this file uses `goto` exactly ONCE, for the landing page, and reaches every
 * page by CLICKING. If the nav is removed, unwired, or pointed at a route
 * that does not exist, these tests fail; nothing else in the repo would notice.
 *
 * ## What each assertion is protecting against
 *
 *  - **Clicking must land on the page, not merely change the URL.** Every click
 *    asserts the destination's own HEADING. A `<Link>` whose target route was
 *    deleted changes the address bar and renders nothing; asserting the URL
 *    alone would pass.
 *  - **The active item must follow the router.** The reference's rule compared
 *    the last path segment to the item id, which breaks on any nested route.
 *  - **LiteLLM must not be offered.** That page is deliberately not ported
 *    (#201 → Bifrost); an entry for it would be dead wiring. Asserted as an
 *    absence here as well as in the unit suite, because the unit assertion is
 *    against the model and this one is against what a browser renders.
 *  - **Collapsing must survive a reload** — a preference that resets on every
 *    page load is not a preference.
 */
import { test as adminTest, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

/**
 * Every nav item, with the heading its page renders. The pairing is the point:
 * a nav label and a page heading that disagree cost the operator a click to
 * find out what the item was, which is why `schedules` is labelled
 * "Schedules & Tasks" here and not the reference's "System".
 */
const ITEMS = [
  { id: 'users', label: 'Users', heading: 'Users', path: '/admin/app/users' },
  { id: 'roles', label: 'Roles', heading: 'Roles', path: '/admin/app/roles' },
  { id: 'projects', label: 'Projects', heading: 'Projects', path: '/admin/app/projects' },
  { id: 'secrets', label: 'Secrets', heading: 'Secrets', path: '/admin/app/secrets' },
  {
    id: 'app-requests',
    label: 'App Requests',
    heading: 'App Requests',
    path: '/admin/app/app-requests',
  },
  {
    id: 'configuration',
    label: 'Configuration',
    heading: 'Configuration',
    path: '/admin/app/configuration',
  },
  { id: 'branding', label: 'Branding', heading: 'Branding', path: '/admin/app/branding' },
  { id: 'email', label: 'E-mail', heading: 'E-mail', path: '/admin/app/email' },
  { id: 'features', label: 'Features', heading: 'Features', path: '/admin/app/features' },
  {
    id: 'service-descriptors',
    label: 'Service Descriptors',
    heading: 'Service Descriptors',
    path: '/admin/app/service-descriptors',
  },
  // Toolkits renders for this persona because shared migration 0114 grants
  // `toolkit_catalogue.type.manage` to the administration `admin` role, which
  // `apps/elitea-web/scripts/e2e-stack.sh` gives e2e-admin. The nav gate reads
  // the same name.
  {
    id: 'toolkit-types',
    label: 'Toolkits',
    heading: 'Toolkits',
    path: '/admin/app/toolkits',
  },
  { id: 'audit', label: 'Audit Trail', heading: 'Audit Trail', path: '/admin/app/audit' },
  {
    id: 'schedules',
    label: 'Schedules & Tasks',
    heading: 'Schedules & Tasks',
    path: '/admin/app/schedules',
  },
  {
    id: 'governance',
    label: 'LLM Governance',
    heading: 'LLM Governance',
    path: '/admin/app/governance',
  },
  // Budgets renders for this persona because shared migration 0062 grants
  // `models.admin.project_budgets.view` to the administration `admin` role,
  // and `apps/elitea-web/scripts/e2e-stack.sh` gives e2e-admin that role. The
  // nav gate reads the same name. An item left out of this list does not make
  // the suite ignore it — the count assertion below fails on it.
  { id: 'budgets', label: 'Budgets', heading: 'Budgets', path: '/admin/app/budgets' },
] as const;

/** The ONE `goto` in this file: the landing page an operator actually opens. */
async function openAdminLanding(page: Page): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/', { waitUntil: 'domcontentloaded' });
  expect(response?.status(), 'the admin SPA must be served at its root').toBeLessThan(400);
  await expect(page.getByTestId('admin-nav')).toBeVisible({ timeout: 20_000 });
}

adminTest('J37: the landing page offers a nav, and marks the page it is showing', async ({ page }) => {
  await openAdminLanding(page);

  // The root route renders the Users page rather than redirecting, so Users is
  // the item that must be marked — without the index alias the LANDING screen
  // would be the one screen with nothing active, which reads as a broken nav on
  // first contact.
  await expect(page.getByRole('heading', { name: 'Users' })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('admin-nav-item-users')).toHaveAttribute('aria-current', 'page');

  // A landmark with a name, not an anonymous <div> of click handlers.
  await expect(page.getByRole('navigation', { name: 'Admin navigation' })).toBeVisible();

  await checkA11y(page);
});

adminTest('J37b: every one of the admin pages is reachable by CLICKING the nav', async ({ page }) => {
  await openAdminLanding(page);
  const nav = page.getByRole('navigation', { name: 'Admin navigation' });

  for (const item of ITEMS) {
    await nav.getByTestId(`admin-nav-item-${item.id}`).click();

    // The URL moved…
    await page.waitForURL((url) => url.pathname === item.path, { timeout: 20_000 });
    // …the destination PAGE rendered — a link to a deleted route would satisfy
    // the URL assertion and render nothing at all…
    await expect(page.getByRole('heading', { name: item.heading, exact: true })).toBeVisible({
      timeout: 20_000,
    });
    // …and the nav says where you are.
    await expect(nav.getByTestId(`admin-nav-item-${item.id}`)).toHaveAttribute(
      'aria-current',
      'page',
    );
  }

  // Exactly the items above. One more would mean an item nothing in this list covers.
  await expect(nav.getByRole('link')).toHaveCount(ITEMS.length);
  await checkA11y(page);
});

adminTest('J37c: the nav offers no LiteLLM entry, because there is no LiteLLM page', async ({ page }) => {
  await openAdminLanding(page);
  const nav = page.getByRole('navigation', { name: 'Admin navigation' });

  await expect(nav.getByText('LiteLLM')).toHaveCount(0);

  // And the route genuinely is not there, so an entry for it would have been a
  // link to nowhere rather than merely a hidden one. The second `goto` in this
  // file, and the only other one: proving a route's ABSENCE is the one thing a
  // click cannot do.
  await page.goto(BASE_URL + '/admin/app/litellm', { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('admin-nav')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole('heading', { name: 'LiteLLM' })).toHaveCount(0);
  // Nothing is marked active either — the nav must not claim you are on a page
  // that does not exist.
  await expect(page.locator('[aria-current="page"]')).toHaveCount(0);
});

adminTest('J37d: keyboard alone reaches a page, because the items are real links', async ({ page }) => {
  await openAdminLanding(page);
  const nav = page.getByRole('navigation', { name: 'Admin navigation' });

  const roles = nav.getByTestId('admin-nav-item-roles');
  // An anchor with an href is what makes this true; the reference's `Box` +
  // `onClick` is not focusable and not activatable by Enter.
  await expect(roles).toHaveAttribute('href', '/admin/app/roles');
  await roles.focus();
  await expect(roles).toBeFocused();
  await page.keyboard.press('Enter');

  await page.waitForURL((url) => url.pathname === '/admin/app/roles', { timeout: 20_000 });
  await expect(page.getByRole('heading', { name: 'Roles' })).toBeVisible({ timeout: 20_000 });
});

adminTest('J37e: collapsing the nav survives a full reload', async ({ page }) => {
  await openAdminLanding(page);
  const nav = page.getByRole('navigation', { name: 'Admin navigation' });

  await expect(nav.getByText('Schedules & Tasks')).toBeVisible();
  await page.getByTestId('admin-nav-collapse-toggle').click();
  await expect(nav.getByText('Schedules & Tasks')).toHaveCount(0);
  // Collapsed hides the LABEL, never the link — the item must stay reachable.
  await expect(nav.getByTestId('admin-nav-item-schedules')).toBeVisible();

  // A FULL RELOAD, not a re-render: an in-memory toggle passes everything above
  // and loses the preference the moment the operator opens a new tab.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('admin-nav')).toBeVisible({ timeout: 20_000 });
  const reloadedNav = page.getByRole('navigation', { name: 'Admin navigation' });
  await expect(reloadedNav.getByText('Schedules & Tasks')).toHaveCount(0);

  // Expanding again is asserted rather than left as cleanup: the preference is
  // per-context localStorage and does not leak into other journeys (each test
  // builds a fresh context from the storage-state FILE), so this is here to
  // prove the toggle works in both directions, not to tidy up.
  await page.getByTestId('admin-nav-collapse-toggle').click();
  await expect(reloadedNav.getByText('Schedules & Tasks')).toBeVisible();
});
