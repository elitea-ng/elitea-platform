/**
 * Journey 37: An operator can REACH every admin page by clicking (JRNY-037)
 *
 * ## Why this journey exists, and why the suite was green without it
 *
 * Unit A14 ported ten admin pages and #223 retired `apps/admin-ui`, making this
 * SPA the one that ships. Its root route was a bare `<Outlet/>`: every page
 * worked by URL and NOTHING in the product linked any of them. An operator
 * opening `/admin/app/` landed on Users and had no way to reach the other nine.
 * (The index is Home under the six areas since ADR-0059 R3.3a, issue 2533.)
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
 *
 * ## Six labelled areas (ADR-0059 decision 6, R3.2, issue 2532)
 *
 * A new install, which the E2E stack is, has the capability document's
 * `admin_console_areas` switch on: the navigation is six labelled areas, each
 * listing the items the document opens, and "Tasks" and "Schedules & Tasks"
 * are one item whose page links both. J37g turns the switch off in its own
 * page only (it rewrites that page's document read, so no other journey sees
 * it) and asserts the two unlabelled groups an upgraded deployment keeps for
 * one release.
 */
import { test as adminTest, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { expectTextSpacingSurvives, expectTypeScale } from '../../fixtures/typeScale';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

/**
 * Every nav item, in navigation order, with its area and the heading its page
 * renders. The pairing is the point: a link whose page does not render its
 * heading is a link to nowhere. e2e-admin holds `admin` (and the custom
 * `e2e_admin_extras`), and its document opens every item the web table places,
 * the Access, Identity providers, SCIM and Client applications pages of R3.6
 * (issue 2537) among them.
 */
const ITEMS = [
  { area: 'people_access', id: 'users', heading: 'Users', path: '/admin/app/users' },
  { area: 'people_access', id: 'access', heading: 'Access', path: '/admin/app/access' },
  {
    area: 'people_access',
    id: 'identity-providers',
    heading: 'Identity providers',
    path: '/admin/app/identity-providers',
  },
  { area: 'people_access', id: 'scim', heading: 'SCIM', path: '/admin/app/scim' },
  { area: 'projects', id: 'projects', heading: 'Projects', path: '/admin/app/projects' },
  { area: 'projects', id: 'app-requests', heading: 'App Requests', path: '/admin/app/app-requests' },
  { area: 'models_spend', id: 'providers-models', heading: 'Providers and models', path: '/admin/app/models/providers' },
  { area: 'models_spend', id: 'models-pricing', heading: 'Models and pricing', path: '/admin/app/models/pricing' },
  { area: 'models_spend', id: 'tool-prices', heading: 'Tool prices', path: '/admin/app/models/tool-prices' },
  { area: 'models_spend', id: 'usage', heading: 'Usage', path: '/admin/app/models/usage' },
  { area: 'models_spend', id: 'logs', heading: 'Logs', path: '/admin/app/models/logs' },
  { area: 'models_spend', id: 'llm-governance', heading: 'LLM Governance', path: '/admin/app/governance' },
  { area: 'models_spend', id: 'budgets-alerts', heading: 'Budgets', path: '/admin/app/budgets' },
  { area: 'agents_governance', id: 'agent-registry', heading: 'Agent registry', path: '/admin/app/agent-registry' },
  {
    area: 'agents_governance',
    id: 'connector-requests',
    heading: 'Connector requests',
    path: '/admin/app/connector-requests',
  },
  {
    area: 'agents_governance',
    id: 'delegation-review',
    heading: 'Delegations awaiting review',
    path: '/admin/app/delegation-review',
  },
  { area: 'agents_governance', id: 'toolkits', heading: 'Toolkits', path: '/admin/app/toolkits' },
  {
    area: 'agents_governance',
    id: 'credential-inventory',
    heading: 'Credential inventory',
    path: '/admin/app/credential-inventory',
  },
  {
    area: 'agents_governance',
    id: 'permission-trimming',
    heading: 'Permission trimming',
    path: '/admin/app/permission-trimming',
  },
  { area: 'agents_governance', id: 'kill-switch', heading: 'Kill switch', path: '/admin/app/kill-switch' },
  { area: 'security', id: 'audit-trail', heading: 'Audit Trail', path: '/admin/app/audit' },
  { area: 'security', id: 'data-retention', heading: 'Data retention', path: '/admin/app/retention' },
  { area: 'platform', id: 'configuration', heading: 'Configuration', path: '/admin/app/configuration' },
  { area: 'platform', id: 'features', heading: 'Features', path: '/admin/app/features' },
  { area: 'platform', id: 'branding', heading: 'Branding', path: '/admin/app/branding' },
  { area: 'platform', id: 'email', heading: 'E-mail', path: '/admin/app/email' },
  { area: 'platform', id: 'schedules-tasks', heading: 'Schedules & Tasks', path: '/admin/app/schedules' },
  {
    area: 'platform',
    id: 'service-descriptors',
    heading: 'Service Descriptors',
    path: '/admin/app/service-descriptors',
  },
  { area: 'platform', id: 'secrets', heading: 'Secrets', path: '/admin/app/secrets' },
  {
    area: 'platform',
    id: 'client-applications',
    heading: 'Client applications',
    path: '/admin/app/client-applications',
  },
] as const;

const AREAS = [
  { id: 'people_access', label: 'People and access' },
  { id: 'projects', label: 'Projects' },
  { id: 'models_spend', label: 'Models and spend' },
  { id: 'agents_governance', label: 'Agents and governance' },
  { id: 'security', label: 'Security' },
  { id: 'platform', label: 'Platform' },
] as const;

/**
 * The two unlabelled groups an upgraded deployment keeps while the switch is
 * off, in their order: what J37g asserts.
 */
const TWO_GROUP_ITEMS = [
  ['users', 'roles', 'access', 'identity-providers', 'scim', 'projects', 'secrets', 'app-requests'],
  [
    'configuration',
    'client-applications',
    'branding',
    'email',
    'features',
    'service-descriptors',
    'toolkit-types',
    'governance',
    'agent-registry',
    'connector-requests',
    'delegation-review',
    'credential-inventory',
    'permission-trimming',
    'budgets',
    'audit',
    'retention',
    'tasks',
    'kill-switch',
    'schedules',
  ],
] as const;

/** The ONE `goto` in this file: the landing page an operator actually opens. */
async function openAdminLanding(page: Page): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/', { waitUntil: 'domcontentloaded' });
  expect(response?.status(), 'the admin SPA must be served at its root').toBeLessThan(400);
  await expect(page.getByTestId('admin-nav')).toBeVisible({ timeout: 20_000 });
}

adminTest('J37: the landing page is Home, beside a nav that marks the page it is showing', async ({ page }) => {
  await openAdminLanding(page);

  // The index is Home under the six areas (ADR-0059 R3.3a, issue 2533): a
  // card per area the document opens, and the queues the caller decides.
  // Home is no item's page, so no item is marked on it.
  await expect(page.getByRole('heading', { level: 1, name: 'Home' })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('admin-home-area-platform')).toBeVisible();
  await expect(page.getByTestId('admin-home-queue-app-requests')).toBeVisible();
  await expect(page.getByRole('navigation', { name: 'Admin navigation' }).locator('[aria-current="page"]')).toHaveCount(0);

  // A card's link opens its page, and the nav marks it there.
  await page.getByTestId('admin-home-item-users').click();
  await page.waitForURL((url) => url.pathname === '/admin/app/users', { timeout: 20_000 });
  await expect(page.getByRole('heading', { level: 1, name: 'Users' })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('admin-nav-item-users')).toHaveAttribute('aria-current', 'page');

  // A landmark with a name, not an anonymous <div> of click handlers.
  await expect(page.getByRole('navigation', { name: 'Admin navigation' })).toBeVisible();

  await checkA11y(page);
});

adminTest('J37b: every one of the admin pages is reachable by CLICKING the nav', async ({ page }) => {
  /*
   * EVERY ADMIN PAGE AND AN AXE PASS, in one test, on purpose: "the nav reaches
   * every page" is one claim, and splitting it per item would let a nav that
   * forgets an item still report the rest green.
   *
   * The default 30 s budget is for a test that opens ONE screen. This one loads
   * every admin route, each with its own listing request, and then runs the
   * accessibility pass over the last of them. On webkit it reached the final
   * item, `Budgets`, and ran out of time inside `checkA11y` — the timing the
   * indexing journey already hit for the same reason. The budget names what the
   * test does instead of trimming what it proves.
   */
  adminTest.setTimeout(120_000);
  await openAdminLanding(page);
  const nav = page.getByRole('navigation', { name: 'Admin navigation' });

  for (const item of ITEMS) {
    await nav.getByTestId(`admin-nav-item-${item.id}`).click();

    // The URL moved…
    await page.waitForURL((url) => url.pathname === item.path, { timeout: 20_000 });
    // …the destination PAGE rendered — a link to a deleted route would satisfy
    // the URL assertion and render nothing at all. Level 1: an area label
    // ("Projects", "Security") is a heading of the navigation too…
    await expect(page.getByRole('heading', { level: 1, name: item.heading, exact: true })).toBeVisible({
      timeout: 20_000,
    });
    // …and the nav says where you are.
    await expect(nav.getByTestId(`admin-nav-item-${item.id}`)).toHaveAttribute(
      'aria-current',
      'page',
    );
  }

  // Exactly the items above, each under its area's label. One more would
  // mean an item nothing in this list covers.
  await expect(nav.getByRole('link')).toHaveCount(ITEMS.length);
  for (const area of AREAS) {
    const section = nav.getByTestId(`admin-nav-area-${area.id}`);
    await expect(section.getByRole('heading', { name: area.label, exact: true })).toBeVisible();
    await expect(section.getByRole('link')).toHaveCount(ITEMS.filter((item) => item.area === area.id).length);
  }

  // "Schedules and tasks" is one item over two pages: the Tasks tab reaches
  // `/tasks`, and the same item stays marked.
  await nav.getByTestId('admin-nav-item-schedules-tasks').click();
  await page.getByTestId('admin-schedules-tasks-tab-tasks').click();
  await page.waitForURL((url) => url.pathname === '/admin/app/tasks', { timeout: 20_000 });
  await expect(page.getByRole('heading', { level: 1, name: 'Tasks', exact: true })).toBeVisible({ timeout: 20_000 });
  await expect(nav.getByTestId('admin-nav-item-schedules-tasks')).toHaveAttribute('aria-current', 'page');
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

  const projects = nav.getByTestId('admin-nav-item-projects');
  // An anchor with an href is what makes this true; the reference's `Box` +
  // `onClick` is not focusable and not activatable by Enter.
  await expect(projects).toHaveAttribute('href', '/admin/app/projects');
  await projects.focus();
  await expect(projects).toBeFocused();
  await page.keyboard.press('Enter');

  await page.waitForURL((url) => url.pathname === '/admin/app/projects', { timeout: 20_000 });
  await expect(page.getByRole('heading', { level: 1, name: 'Projects', exact: true })).toBeVisible({ timeout: 20_000 });
});

adminTest('J37e: collapsing the nav survives a full reload', async ({ page }) => {
  await openAdminLanding(page);
  const nav = page.getByRole('navigation', { name: 'Admin navigation' });

  await expect(nav.getByText('Schedules and tasks')).toBeVisible();
  await page.getByTestId('admin-nav-collapse-toggle').click();
  await expect(nav.getByText('Schedules and tasks')).toHaveCount(0);
  // Collapsed hides the LABEL, never the link — the item must stay reachable.
  await expect(nav.getByTestId('admin-nav-item-schedules-tasks')).toBeVisible();

  // A FULL RELOAD, not a re-render: an in-memory toggle passes everything above
  // and loses the preference the moment the operator opens a new tab.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('admin-nav')).toBeVisible({ timeout: 20_000 });
  const reloadedNav = page.getByRole('navigation', { name: 'Admin navigation' });
  await expect(reloadedNav.getByText('Schedules and tasks')).toHaveCount(0);

  // Expanding again is asserted rather than left as cleanup: the preference is
  // per-context localStorage and does not leak into other journeys (each test
  // builds a fresh context from the storage-state FILE), so this is here to
  // prove the toggle works in both directions, not to tidy up.
  await page.getByTestId('admin-nav-collapse-toggle').click();
  await expect(reloadedNav.getByText('Schedules and tasks')).toBeVisible();
});

/*
 * Typography spec rev. 2 §4.6: ONE type scale for the console. Every admin
 * page reached through the nav must paint its text at a rung of the default
 * pack's ladder (12/14/16/20px) — the unit and Storybook suites prove the
 * theme and the component overrides, but only a real page shows a call site
 * that sizes its own text. On the landing page the WCAG 1.4.12 text-spacing
 * overrides must not make a button, chip or nav item clip its label.
 */
adminTest('J37f: every admin page renders on the one type scale', async ({ page }) => {
  adminTest.setTimeout(120_000);
  await openAdminLanding(page);
  await expect(page.getByRole('heading', { level: 1, name: 'Home' })).toBeVisible({ timeout: 20_000 });
  await expectTextSpacingSurvives(page, 'admin landing');

  const nav = page.getByRole('navigation', { name: 'Admin navigation' });
  for (const item of ITEMS) {
    await nav.getByTestId(`admin-nav-item-${item.id}`).click();
    await expect(page.getByRole('heading', { level: 1, name: item.heading, exact: true })).toBeVisible({
      timeout: 20_000,
    });
    await expectTypeScale(page, item.path);
  }
});

/*
 * The switch off: an upgraded deployment keeps the two unlabelled groups for
 * one release (the upgrade note says so). The switch is a platform setting,
 * and writing it would change the console of every journey running beside
 * this one, so this page alone reads a document whose switch is off: the
 * route answers the server's own document with the one field changed.
 */
adminTest('J37g: with admin_console_areas off, the two unlabelled groups render unchanged', async ({ page }) => {
  await page.route('**/api/v2/admin/console', async (route) => {
    const response = await route.fetch();
    const document = (await response.json()) as Record<string, unknown>;
    expect(document['admin_console_areas'], 'a new install has the six areas on').toBe(true);
    await route.fulfill({ response, json: { ...document, admin_console_areas: false } });
  });
  await openAdminLanding(page);
  const nav = page.getByRole('navigation', { name: 'Admin navigation' });

  await expect(nav.getByTestId(/^admin-nav-area-/)).toHaveCount(0);
  const links = nav.getByRole('link');
  await expect(links).toHaveCount(TWO_GROUP_ITEMS.flat().length);
  const ids = await links.evaluateAll((anchors) => anchors.map((anchor) => anchor.getAttribute('data-testid')));
  expect(ids).toEqual(TWO_GROUP_ITEMS.flat().map((id) => `admin-nav-item-${id}`));
  await expect(nav.getByText('Schedules & Tasks')).toBeVisible();

  // Both groups' pages still open by clicking, Roles among them.
  await nav.getByTestId('admin-nav-item-roles').click();
  await page.waitForURL((url) => url.pathname === '/admin/app/roles', { timeout: 20_000 });
  await expect(page.getByRole('heading', { level: 1, name: 'Roles', exact: true })).toBeVisible({ timeout: 20_000 });
  await nav.getByTestId('admin-nav-item-tasks').click();
  await page.waitForURL((url) => url.pathname === '/admin/app/tasks', { timeout: 20_000 });
  await expect(page.getByRole('heading', { level: 1, name: 'Tasks', exact: true })).toBeVisible({ timeout: 20_000 });
  // The Tasks page shows no Schedules/Tasks strip with the switch off.
  await expect(page.getByTestId('admin-schedules-tasks-tabs')).toHaveCount(0);
});
