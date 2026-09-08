/**
 * Snapshot coverage for the ADMIN SPA (`/admin/app/**`) — issue #229.
 *
 * ## Why this file exists
 *
 * `parity/screenshot-index.json` carried no `/admin/app/*` row, and
 * `scripts/check-visual-coverage.mjs` enforces only routes the index knows
 * about. So the visual gate could not fail for an admin screen — not "did not",
 * *could not*, for any change, ever. #225 added persistent left-hand navigation
 * to all ten admin pages and altered zero snapshots, with both `Visual
 * regression` and `check-visual-coverage` green.
 *
 * That was defensible while `/admin/app/` was served by the separate `admin_ui`
 * SPA. It stopped being defensible when #200 ported ten real admin pages into
 * this app and #223 retired the submodule and pinned the published image at
 * `target: final` (which is `FROM e2e`): the admin SPA this bundle builds is the
 * one that ships.
 *
 * ## The admin shell is NOT the app shell
 *
 * `routes.visual.spec.ts`'s `shellSettled()` waits on a permission-gated sidebar
 * link and the resolved project name, because the main app's sidebar renders
 * SHORTER while `GET /auth/permissions/...` is in flight. The admin nav has no
 * such hazard and the difference is structural, not incidental:
 * `adminNavItems.visibleAdminNavGroups()` filters on
 * `window.admin_ui_config.permissions`, which the Go adminui handler substitutes
 * into the HTML at request time (`src/pages/admin/adminUiConfig.ts`). There is
 * no query behind the nav, so it is at full length on first paint.
 *
 * What DOES vary is the rail width: `adminNavCollapsed.ts` persists the
 * collapsed flag in `localStorage['el.admin.nav.collapsed']`. Each test builds a
 * fresh context from the storage-state FILE, so nothing leaks between tests
 * here — but `adminShellSettled()` asserts the expanded state anyway rather than
 * assuming it, since a collapsed rail changes the width of every pixel to its
 * right and would look like a page-level regression.
 *
 * ## Landmarks, and how each was proven
 *
 * Same rule as `routes.visual.spec.ts`: a landmark that also renders during load
 * pins a loading state as the reference and then matches forever (#159, #174).
 * Every landmark below was MEASURED with a stall experiment — `page.route` the
 * page's own `**\/api\/v2\/admin\/**` (or `**\/elitea_core\/admin\/**`) calls
 * into a five-minute stall, load the route, and check whether the locator still
 * becomes visible. Results are recorded per entry.
 *
 * The admin pages need no shell-allowlist carve-out (method note 1 in
 * `routes.visual.spec.ts`), precisely because the nav is not query-driven: there
 * is no shell request to accidentally stall.
 *
 * Four traps this page set found, all recorded because the obvious landmark was
 * wrong in each case:
 *
 *  - Users / Projects / Secrets / App Requests read their tab counts from a
 *    listing whose default is `{platform: 0, system: 0}`-shaped, so the tabs
 *    render `Platform Users (0)` etc. DURING LOAD. A count regex `\(\d+\)`
 *    matches the loading state.
 *  - Their pagination footers render `0–0 / 0` during load, for the same reason.
 *  - Configuration and Features render "This deployment publishes no
 *    configuration sections." while `sections` is still `[]` — the empty state
 *    and the loading state are the same string.
 *  - Every one of these pages renders its DataGrid (headers included) with the
 *    stock loading overlay, so `getByRole('grid')`, `getByRole('columnheader')`
 *    and `getByRole('table')` on a DataGrid page are all present during load.
 *    The two `<Table>`-based pages (Roles, Schedules) are different: they have
 *    an explicit textual loading branch and mount no table at all until the
 *    query resolves, which is why `getByRole('table', { name: … })` IS a valid
 *    landmark there and is not one on the DataGrid pages.
 *
 * ## Personal data
 *
 * Admin pages render names and email addresses, and a committed PNG is
 * permanent. Every identity in these baselines comes from
 * `scripts/e2e-stack.sh seed` and is synthetic: `e2e-admin@autotest.local`,
 * `e2e-member@autotest.local`, `e2e-project-owner@autotest.local`,
 * `e2e-project-admin@autotest.local`, and the display names `E2E Admin`,
 * `E2E Member`, `E2E Project Owner`, `E2E Project Admin`. The nav footer shows
 * the signed-in operator, which is the admin persona's own synthetic address.
 * Nothing here is read from a developer's local database — the `@visual`
 * project runs only against the standalone E2E stack, whose Postgres volume is
 * created and seeded by that script.
 */
import { test, expect, type Locator, type Page } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../playwright.config';
import { SNAPSHOT_TOLERANCE, settle, volatileRegions } from './lib/settle';

/**
 * The admin pages are gated server-side on `administration`-mode permissions
 * that only the admin persona holds (`admin.auth.users`,
 * `models.admin.audit_trail.view`, …). The `visual` project's default
 * storageState is the MEMBER persona, which is refused the user listing
 * outright (journey J33) — so without this every shot below would be a
 * photograph of a 403.
 */
test.use({ storageState: STORAGE_STATE.admin });

/** Volatile regions specific to the admin tables, on top of the shared list. */
function adminVolatileRegions(page: Page): Locator[] {
  return [
    ...volatileRegions(page),
    // `Last login` — `new Date(value).toLocaleString()` in the browser's locale
    // and timezone (`AdminUsersTable.tsx`'s `formatLastLogin`). The value moves
    // every time a persona authenticates, and `e2e/auth.setup.ts` authenticates
    // both personas at the start of every run, so this cell is different on
    // each execution by construction. Scoped to `.MuiDataGrid-cell` so the
    // column HEADER stays compared — masking the header would hide the column
    // disappearing.
    page.locator('.MuiDataGrid-cell[data-field="last_login"]'),
    // `Requested At` on App Requests — same formatter, and the seed stamps the
    // probe rows at seed time, so it is the wall clock of whenever the stack
    // was provisioned.
    page.locator('.MuiDataGrid-cell[data-field="created_at"]'),
  ];
}

/**
 * The admin shell, settled and pinned.
 *
 * `admin-nav` proves the SPA mounted (the Go handler serves the same
 * `index.html` for every path under the basepath, so a 200 alone proves
 * nothing about the router). The expanded-rail assertion is the load-bearing
 * half: `SchedulesTasks`' nav label is rendered only when the rail is expanded,
 * so its visibility is the inverse of the collapsed state, read off the real
 * DOM rather than off `localStorage`.
 */
async function adminShellSettled(page: Page): Promise<void> {
  await expect(page.getByTestId('admin-nav')).toBeVisible({ timeout: 20_000 });
  await expect(
    page.getByRole('navigation', { name: 'Admin navigation' }).getByText('Schedules & Tasks'),
  ).toBeVisible({ timeout: 20_000 });
}

/**
 * No indeterminate `LinearProgress` may be on screen when the shutter opens.
 *
 * Four of these pages show one (`AppRequests`, `Configuration`, `Features`,
 * `ServiceDescriptors` unconditionally; `Roles` and `SchedulesTasks` while
 * fetching). It is an ANIMATED bar, so a shot taken mid-animation diffs against
 * itself, and on `SchedulesTasks` it is not inside a fixed-height box — its
 * presence adds ~4px of layout height, which would make the baseline taller
 * than the steady state it is supposed to represent.
 *
 * Asserted separately from the landmark because the two are not the same claim:
 * a landmark says "this page's data arrived", this says "nothing is still in
 * flight". `Features` needs both — its section list and its section VALUES are
 * two queries, and the first resolving is what makes the second start.
 */
async function noProgressBar(page: Page): Promise<void> {
  await expect(page.locator('.MuiLinearProgress-root')).toHaveCount(0, { timeout: 20_000 });
}

interface AdminVisualRoute {
  readonly name: string;
  /** The `parity/screenshot-index.json` route, which for these is also the URL. */
  readonly path: string;
  /** A landmark a loading state cannot render. Measured, never chosen. */
  readonly landmark: (page: Page) => Locator;
  /** Also capture a light-scheme variant. */
  readonly light?: boolean;
}

/*
 * Each entry carries an `@covers <route>` annotation naming the
 * `parity/screenshot-index.json` route it claims, verbatim.
 * `scripts/check-visual-coverage.mjs` counts ONLY exact matches — coverage here
 * is DECLARED, never inferred from navigation, because inference over-reported
 * and laundered the gap it existed to expose.
 */
const ADMIN_ROUTES: readonly AdminVisualRoute[] = [
  {
    // @covers /admin/app/users
    name: 'admin-users',
    path: '/admin/app/users',
    // A seeded row's email, scoped to `<main>`. NOT the DataGrid, its column
    // headers, the tab counts or the pagination text: measured, all four render
    // under a sustained stall (`rows=[]` + `loading` still draws the header row
    // and the stock overlay; `counts` defaults to zeros so the tabs read
    // `Platform Users (0)` / `System Users (0)`; the footer reads `0–0 / 0`).
    // The scope matters as well as the locator: the nav footer shows the
    // signed-in operator, who IS `e2e-admin@autotest.local`, so an unscoped
    // `getByText` matches the chrome and resolves during load too.
    // Measured: loaded YES, stalled no.
    landmark: (page) => page.getByRole('main').getByText('e2e-admin@autotest.local'),
    light: true,
  },
  {
    // @covers /admin/app/roles
    name: 'admin-roles',
    path: '/admin/app/roles',
    // The permission matrix table. This page has an explicit textual loading
    // branch — `Loading permissions…` renders while the draft `rows` is
    // `undefined` — and the table is mounted only in the resolved branch, so
    // the two are mutually exclusive by construction.
    // Measured: loaded YES, stalled no (`Loading permissions…` is the inverse:
    // loaded no, stalled YES).
    landmark: (page) => page.getByRole('table', { name: 'Permission matrix' }),
  },
  {
    // @covers /admin/app/projects
    name: 'admin-projects',
    path: '/admin/app/projects',
    // A seeded project name (`e2e-team-active`, from the admin projects
    // fixture). Same reasoning as Users: this page's DataGrid, tab counts
    // (`Team Projects (0)`) and pagination all render during load.
    // Measured: loaded YES, stalled no.
    landmark: (page) => page.getByText('e2e-team-active'),
  },
  {
    // @covers /admin/app/secrets
    name: 'admin-secrets',
    path: '/admin/app/secrets',
    // A seeded key of the GLOBAL vault (`centry.secrets_data`'s `admin` row —
    // the five chat upload limits). This page has no empty-guard at all: the
    // DataGrid is always mounted, so `No secrets found` is its `noRowsLabel`
    // and the grid itself is present under load.
    //
    // Row ORDER is deterministic and that is checked rather than assumed: Go
    // map iteration is randomised, and `secrets/admin.go`'s `sortByName` exists
    // precisely to stop that reaching the listing.
    // Measured: loaded YES, stalled no.
    landmark: (page) => page.getByText('chat_max_upload_count', { exact: true }),
  },
  {
    // @covers /admin/app/schedules
    name: 'admin-schedules',
    path: '/admin/app/schedules',
    // The schedules table. Like Roles, this page has a textual loading branch
    // (`Loading schedules…`) and mounts no table until the query resolves.
    //
    // The `Last run` column is NOT masked, deliberately. The two seeded probe
    // rows are inactive with `last_run` NULL, and the E2E stack runs no
    // scheduler (`deploy/docker-compose.e2e-standalone.yml` has no
    // elitea-scheduler service), so the cell reads the literal `Never` on every
    // run. Masking it would hide the one thing worth seeing there.
    // Measured: loaded YES, stalled no.
    landmark: (page) => page.getByRole('table', { name: 'Schedules' }),
  },
  {
    // @covers /admin/app/tasks
    name: 'admin-tasks',
    path: '/admin/app/tasks',
    // The row COUNT, not the table and not the empty text.
    //
    // This stack runs no background jobs, so the union is empty and the table
    // is not mounted at all — there is nothing with a table role to wait for.
    // The empty text would be wrong for the opposite reason: it renders only
    // once the query has resolved, but so does `Jobs: 0`, and the count is the
    // one element present in BOTH the empty and the populated screen, so this
    // landmark keeps working the day the stack does run a job.
    //
    // No mask: every value on the empty screen is a constant. A populated one
    // would need `Started`/`Finished` masked, and this note is here so that
    // fact is not rediscovered.
    // Measured: loaded YES, stalled no.
    landmark: (page) => page.getByTestId('admin-tasks-count'),
  },
  {
    // @covers /admin/app/app-requests
    name: 'admin-app-requests',
    path: '/admin/app/app-requests',
    // The seeded pending request's description. The `No app requests` empty
    // state is guarded by `!isLoading` and so would also do — but the seed
    // plants two pending rows, so the populated branch is what this stack
    // actually renders and an empty-state landmark would never resolve.
    // Measured: loaded YES, stalled no.
    landmark: (page) => page.getByText('E2E probe: please enable this catalogue entry.').first(),
  },
  {
    // @covers /admin/app/configuration
    name: 'admin-configuration',
    path: '/admin/app/configuration',
    // The Guardrails form's own Save control.
    //
    // This used to be `admin-configuration-unavailable`, the 501 refusal Alert,
    // because the page's first section (`guardrails`, order 1) had no backend.
    // It has one now, so the landing screen is a FORM and that testid no longer
    // exists anywhere on it — which is why this landmark had to move rather
    // than the baseline simply being regenerated.
    //
    // Save is the right replacement for the same reason the Alert was: it
    // renders only once the section's values have loaded, so it cannot be
    // mistaken for a loading state. NOT the "This deployment publishes no
    // configuration sections." text: `activeSection` is `undefined` while
    // `sections` is still `[]`, so that string is the LOADING state as well as
    // the empty one.
    // Measured: loaded YES, stalled no.
    //
    // NOT the MCP catalogue editor, even though the MCP Servers section also
    // became editable. The landing section is the first one the page can serve
    // and `guardrails` is order 1, so Guardrails keeps the slot; the catalogue
    // is one click away and journey J34e covers it.
    landmark: (page) => page.getByRole('button', { name: 'Save' }),
    light: true,
  },
  {
    // @covers /admin/app/features
    name: 'admin-features',
    path: '/admin/app/features',
    // The `Enable MCP` switch, from the MCP Configuration section — the first
    // AVAILABLE section, which is what the page selects by default. Reaching it
    // requires BOTH queries to have resolved (the section list, then that
    // section's values), which is exactly the point: the intermediate state
    // renders the literal `Loading feature settings…`.
    // Measured: loaded YES, stalled no.
    landmark: (page) => page.getByRole('switch', { name: 'Enable MCP' }),
  },
  {
    // @covers /admin/app/governance
    name: 'admin-governance',
    path: '/admin/app/governance',
    // The seeded `budget_alert` row's name cell.
    //
    // NOT the DataGrid's no-rows label, which was the first choice and was
    // wrong: this stack seeds one governance row (the #322 platform soft-alert
    // config), so that label never renders and the landmark could not resolve.
    // NOT the propagation Alert either — it states a property of the GATEWAY,
    // not of the query, so it is present in the loading frame too and would
    // pin a loading state as the reference (#159, #174).
    //
    // A cell of a seeded row is the one thing here that proves the list query
    // RESOLVED. Measured: loaded YES, stalled no.
    landmark: (page) => page.getByRole('gridcell', { name: 'global', exact: true }),
  },
  {
    // @covers /admin/app/branding
    name: 'admin-branding',
    path: '/admin/app/branding',
    // The product-name input. The page renders its FORM only once the
    // settings query resolved (`state.isLoaded`); during load it shows the
    // heading, the subtitle and a LinearProgress and nothing else, so no
    // field can be mistaken for a loading state. The two preview surfaces
    // are gated the same way and would do as well; the input is the smaller
    // locator. NOT measured with the stall experiment the entries above
    // record (this change ran without the E2E stack); the claim rests on the
    // `isLoaded` gate in Branding.tsx, which unmounts the whole form until
    // the query resolves.
    landmark: (page) => page.getByRole('textbox', { name: 'Product name' }),
  },
  {
    // @covers /admin/app/email
    name: 'admin-email',
    path: '/admin/app/email',
    // The SMTP host input. The page renders its form only once the settings
    // query resolved (`AdminEmailEditor` returns a bare `LinearProgress` while
    // `state.isLoading` or the draft has not been seeded), so no field here can
    // be mistaken for a loading state.
    //
    // NOT the "not configured" Alert: this stack configures no relay, so that
    // Alert is the honest reference — but it is also the sentence the server
    // gives, and the point of a landmark is to prove the QUERY resolved. The
    // Alert renders from `state.data`, so it would do; the input is the
    // smaller locator and matches what the Branding entry above uses.
    landmark: (page) => page.getByRole('textbox', { name: 'SMTP host' }),
  },
  {
    // @covers /admin/app/budgets
    name: 'admin-budgets',
    path: '/admin/app/budgets',
    // The "Team (N)" tab. Its label carries the per-type COUNT the list query
    // returns, so the tab text settles only once that query resolved; the
    // bare "All" tab renders before any data and would prove nothing. The
    // E2E gateway holds no NATS counter, so the enforcement-off banner above
    // the table is the honest default state of this shot, not an edge case.
    landmark: (page) => page.getByRole('tab', { name: /^Team \(\d+\)$/ }),
  },
  {
    // @covers /admin/app/service-descriptors
    name: 'admin-service-descriptors',
    path: '/admin/app/service-descriptors',
    // The GRID, which this page renders only on `query.isSuccess` — during
    // load it shows its title and a `LinearProgress` and nothing else, so the
    // grid cannot be mistaken for a loading state.
    //
    // It used to be the 501 Alert. The admission plane replaced that refusal
    // with a real listing (migration 0107), so the Alert no longer renders and
    // a landmark still waiting for it fails against a page that is working —
    // which is what turned this spec red rather than any pixel changing.
    //
    // THE SECOND DATA ROW, and not the grid alone. The grid was enough while
    // this stack registered nothing, because "grid present" and "grid final"
    // were the same state. They no longer are: `scripts/e2e-stack.sh seed`
    // restarts elitea-main after writing its project rows, so the Inventory
    // and DeepWiki facades' boot-time registrars file an origin and an
    // inactive revision each, and the reference is now a listing with two
    // rows in it. Both are filed from goroutines started at boot, so a shot
    // taken on the grid alone could catch the page one registration early and
    // record whichever half had landed.
    //
    // Row 0 is the column header (`rowgroup > row` in the accessibility tree),
    // so `nth(2)` is the SECOND registered provider. Waiting for it waits for
    // the whole listing without naming a provider — the names come from the
    // providers' own descriptors ("wikis", not "deepwiki") and are theirs to
    // change.
    //
    // Not the empty-state text: the DataGrid's `noRowsLabel` renders for an
    // empty list AND is what an unmounted-rows state would show. On this stack
    // it is now also the WRONG reference — a page that says "nothing is
    // registered" here would mean the admission plane never heard from the
    // facades this deployment composes.
    landmark: (page) =>
      page.getByRole('grid', { name: 'Registered service descriptors' }).getByRole('row').nth(2),
  },
  {
    // @covers /admin/app/toolkits
    name: 'admin-toolkit-types',
    path: '/admin/app/toolkits',
    // The GRID, which this page renders only on `query.isSuccess` - during load
    // it shows its title, its intro paragraph and a `LinearProgress`, so the
    // grid cannot be mistaken for a loading state. The filter row renders in
    // the same branch, so waiting on the grid also waits on it.
    //
    // Not the empty-state text: the DataGrid's `noRowsLabel` renders for an
    // empty list AND for a page whose rows have not mounted, while the grid
    // itself is present only after the listing resolved. This stack records no
    // policy rows, so the reference is every served type at its DEFAULT - which
    // is the state a fresh install has, and the one the absence rule protects.
    // Measured: loaded YES, stalled no.
    landmark: (page) =>
      page.getByRole('grid', { name: 'Toolkit types this platform can serve' }),
  },
];

for (const route of ADMIN_ROUTES) {
  test(`@visual ${route.name}`, async ({ page }) => {
    await page.goto(BASE_URL + route.path, { waitUntil: 'domcontentloaded' });
    await adminShellSettled(page);
    await expect(route.landmark(page).first()).toBeVisible({ timeout: 20_000 });
    await noProgressBar(page);
    await settle(page);

    await expect(page).toHaveScreenshot(`${route.name}.png`, {
      fullPage: false,
      mask: adminVolatileRegions(page),
      ...SNAPSHOT_TOLERANCE,
    });
  });
}

/**
 * Switches to the light colour scheme through the admin SPA's OWN control, on
 * the page under test, then proves the switch took effect.
 *
 * The admin nav header renders `shared/ui/ThemeModeToggle`
 * (`AdminNavHeader.tsx`), so — unlike the main app, whose toggle lives on
 * `/app/settings/personalization` and forces `routes.visual.spec.ts` to
 * navigate away and back — the control is right there. Using it rather than
 * writing the persisted key directly is the same rule #61 sets for the main
 * app's light shots: faking the attribute would make these test the stylesheet
 * while claiming to test the theme mechanism.
 *
 * `data-el-scheme` (`shared/brand/constants.ts`'s `COLOR_SCHEME_ATTRIBUTE`) is
 * what MUI's `colorSchemeSelector` resolves against, so polling it is polling
 * the thing the stylesheet keys off.
 */
async function useLightScheme(page: Page): Promise<void> {
  const light = page
    .getByTestId('admin-nav')
    .getByRole('button', { name: 'Light', exact: true });
  await expect(light).toBeVisible({ timeout: 20_000 });
  await light.click();
  await expect(light).toHaveAttribute('aria-pressed', 'true');
  await expect
    .poll(() => page.evaluate(() => document.documentElement.getAttribute('data-el-scheme')), {
      timeout: 10_000,
    })
    .toBe('light');

  // Undo the click's side effects on the CHROME, or they end up in the
  // baseline: the pointer is left resting on the toggle, so MUI shows its
  // `Light theme` Tooltip, and the button keeps focus, so it draws a focus
  // ring. Both were in the first generated `admin-users-light` PNG. A tooltip
  // in a reference is worse than cosmetic — it appears on a timer, so the
  // baseline would be a race with its own enter delay.
  await page.mouse.move(0, 0);
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await expect(page.getByRole('tooltip')).toHaveCount(0, { timeout: 10_000 });
}

/*
 * Light-scheme variants — two, not nine, and the choice is deliberate.
 *
 * PNGs do not delta-compress, so every baseline costs its full size in history
 * forever (see the budget tripwire in scripts/check-visual-coverage.mjs). Nine
 * more light shots would roughly double this file's contribution for a second
 * copy of the same three widgets. The admin surface is built from exactly two
 * kinds of screen, and one of each is covered here:
 *
 *   admin-users          the DataGrid pages — grid chrome, tabs, status chips,
 *                        pagination, the nav rail beside them
 *   admin-configuration  the section-list pages — nav List, secondary labels,
 *                        and an `Alert severity="info"`, whose light palette is
 *                        a different accent hue entirely (the index's
 *                        `accentHue` finding: magenta/lavender against dark's
 *                        cyan/teal)
 *
 * Roles' `<Table>`, Schedules' switches and the Features form are NOT covered
 * in light. That is a real gap and it is stated rather than implied.
 */
for (const route of ADMIN_ROUTES.filter((r) => r.light)) {
  test(`@visual ${route.name}-light`, async ({ page }) => {
    await page.goto(BASE_URL + route.path, { waitUntil: 'domcontentloaded' });
    await adminShellSettled(page);
    await useLightScheme(page);
    await expect(route.landmark(page).first()).toBeVisible({ timeout: 20_000 });
    // Re-asserted after the landmark rather than only after the click: a
    // re-render that reset the scheme would otherwise produce a second dark
    // baseline under a light name, and it would look correct.
    expect(await page.evaluate(() => document.documentElement.getAttribute('data-el-scheme'))).toBe(
      'light',
    );
    await noProgressBar(page);
    await settle(page);

    await expect(page).toHaveScreenshot(`${route.name}-light.png`, {
      fullPage: false,
      mask: adminVolatileRegions(page),
      ...SNAPSHOT_TOLERANCE,
    });
  });
}

/*
 * ── CLASSIFICATION: why a refusal screen gets a baseline and a stub does not ──
 *
 * `check-visual-coverage.mjs` enforces only `wiringStatus: wired`, on the
 * reasoning that a baseline of scaffolding makes the stub the official
 * reference and goes green forever. Two of the pages above look, from a
 * distance, like exactly that case, and they are not:
 *
 *  - **Service Descriptors** renders nothing but an `Alert`. But the page is
 *    FINISHED: the subsystem it would administer does not exist on this
 *    platform (pylon's provider hub; #201/ADR-0012 replace it with a different
 *    shape), the server answers 501, and the page renders THE SERVER'S OWN
 *    SENTENCE rather than a copy it carries. Pinning it is valuable precisely
 *    because it is the regression this port was built to prevent: if someone
 *    wires that endpoint to a stub returning 200 with an empty array, this
 *    baseline changes. A hardcoded explanation would not have.
 *  - **Configuration** renders its full section list with every section
 *    `Not available here`. Same argument: the section titles and every reason
 *    string come from the server's schema, so the screen is a photograph of
 *    what this deployment actually declares, and it moves the moment a section
 *    becomes available.
 *
 * The distinction from scaffolding is that scaffolding is UNFINISHED UI —
 * `RouteShell`, a heading with nothing under it — where the eventual screen is
 * meant to look different and a baseline would freeze the wrong thing. A
 * refusal is the finished screen. It is also distinct from `/help-center`,
 * which is EXEMPT in the coverage script: there the CONTENT is missing (every
 * card renders "No links configured" because nothing has been configured), not
 * refused, so the baseline would pin an unconfigured tenant rather than a
 * decision.
 *
 * ── NOT COVERED, and why ────────────────────────────────────────────────────
 *
 *  - `/admin/app/audit` (Audit Trail). EXEMPT in
 *    `scripts/check-visual-coverage.mjs`, which prints the reason on every run.
 *    Short version: every visible thing on the screen is a function of the wall
 *    clock at seed time — the `Time` column is second-resolution local time,
 *    the two `DateRangeField`s render today's date, and the heatmap's column
 *    geometry and per-cell alpha are computed from where the rows fall inside
 *    the range. A baseline taken against one stack cannot match a stack seeded
 *    at a different time of day, which is every other stack. Masking is not a
 *    way out: what would have to be masked is the whole content area.
 *    WHICH day it is is no longer on that list: #214 pinned one timezone for
 *    the browser and the seed, and journey 29 freezes its clock to the day the
 *    fixture was written on. The MINUTE the seed ran is what still moves.
 *
 *  - The admin INDEX route (`/admin/app/`). It renders `AdminUsers` — the same
 *    component, deliberately, rather than redirecting (`router.tsx` explains
 *    why) — so a baseline for it would be a second copy of `admin-users` under
 *    a different name. Journey 37 already asserts that the index marks the
 *    Users nav item active, which is the only thing that distinguishes them.
 *
 *  - The collapsed rail. `chat-empty-rail-collapsed` covers the equivalent
 *    state for the main app's sidebar; journey J37e asserts the admin rail
 *    collapses, survives a reload, and keeps its links reachable. A snapshot
 *    would add a PNG without adding a claim either of those does not make.
 *
 *  - Every dialog and drawer (`admin-secret-dialog`, `ProjectMemberDialog`,
 *    `ProjectActivityDrawer`, `ScheduleHistoryDrawer`) and the non-default tabs
 *    (`System Users`, `Internal` secrets, `Tasks`/`Active Tasks`, the three
 *    non-`Pending` App Requests tabs, the three non-default Roles scopes).
 *    Reachable and worth covering eventually; each is another PNG against the
 *    12MB budget and none is covered by a claim made here.
 */
