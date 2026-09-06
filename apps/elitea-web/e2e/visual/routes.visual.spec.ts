/**
 * Snapshot coverage for the routes `parity/screenshot-index.json` marks
 * `wiringStatus: wired` — the only ones whose UI is real enough that a baseline
 * means something. See `e2e/visual/README.md` before adding to this file, in
 * particular the rule that baselines are only ever generated inside
 * `mcr.microsoft.com/playwright:v1.62.0-noble`.
 *
 * Each spec navigates, waits for the SHELL to resolve, waits for a landmark
 * that only this route's resolved data can produce, masks the regions that
 * legitimately vary run to run, and snapshots.
 *
 * This file covers the MAIN app only (`/app/**`). The admin SPA
 * (`/admin/app/**`) is in `admin.visual.spec.ts`, which needs a different
 * persona and a different shell guard — see that file's header and the README.
 * Shared helpers are in `lib/settle.ts`; they are deliberately not imported
 * from here, because importing a `*.spec.ts` re-registers its tests.
 *
 * ── The landmark rule, and how every landmark here was proven ───────────────
 *
 * `toHaveScreenshot` on a page that has not finished rendering produces a
 * baseline of a spinner, and then every future run matches it. #159 found the
 * `settings-analytics` baseline was exactly that; #174 found the same latent
 * defect on two more routes.
 *
 * So no landmark is admitted here on inspection. Each one below was MEASURED
 * with a stall experiment: load the route with its own API responses stalled
 * for five minutes and check whether the landmark still becomes visible. A
 * landmark that resolves under the stall cannot tell loading from loaded and is
 * rejected. Each entry records its own result.
 *
 * TWO METHOD NOTES, because both produced false passes before they were caught,
 * and anyone re-running these experiments will hit them again:
 *
 *  1. STALL THE ROUTE'S OWN API, NOT THE SHELL'S. Stalling every `**\/api\/**`
 *     call also stalls `/projects/project/default/`, so no project is ever
 *     selected — and every page query is `enabled: projectId !== undefined`.
 *     The queries then never START, so the page renders its no-data branch
 *     immediately and the landmark looks like it resolved under load. Measured
 *     on `/settings/secrets`: a blanket stall showed the resolved-looking
 *     "No secrets" empty state, while a shell-allowed stall shows a row
 *     SKELETON and no "No secrets" at all. The shell's four endpoints
 *     (`/branding/`, `/projects/project/default/`, `/auth/permissions/`,
 *     `/social/author`) must be let through.
 *  2. THE STALL MUST OUTLAST THE WHOLE EXPERIMENT. A stall that resolves or
 *     aborts part-way lets the query fail, and a failed query renders the same
 *     error/empty branch as a loaded one. Checking four candidate landmarks at
 *     20s each against a 60s stall put every later check past the abort, and
 *     `/chat` reported all four resolving under load when in fact none do.
 *
 * ── The shell is part of every shot ────────────────────────────────────────
 *
 * `shellSettled()` runs before every snapshot in this file. #174 flagged the
 * app shell as the residual exposure common to every route, and it is worse
 * than "the project name arrives late": the sidebar's nav list is
 * PERMISSION-FILTERED (`widgets/sidebar/lib/navSections.ts`'s
 * `visibleNavSections`), and `usePermissionSet` returns an EMPTY SET until
 * `GET /auth/permissions/prompt_lib/{id}` resolves. Every permission-gated nav
 * item is therefore absent during load — the sidebar renders SHORTER, with no
 * spinner and no skeleton to give it away. `ProjectSwitcher` has the same
 * shape: until the project list resolves it renders the literal "No projects"
 * and a placeholder avatar, which looks like a legitimately-rendered screen.
 *
 * Measured (shell-allowed stall vs. full stall, on
 * `/settings/create-personal-token`, which has no data of its own):
 *   Credentials nav link       loaded YES  stalled no   ← permission-gated
 *   resolved project name      loaded YES  stalled no
 *   "No projects" fallback     loaded no   stalled YES
 *   Applications nav link      loaded YES  stalled YES  ← ungated, no good
 *   sidebar-create-button      loaded YES  stalled YES  ← always present
 * The first two are the landmark; the last two are why the obvious choices are
 * not.
 */
import { test, expect, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { SNAPSHOT_TOLERANCE, selectProject, settle, shellSettled, volatileRegions } from './lib/settle';

/*
 * `shellSettled()` now lives in `./lib/settle.ts`, imported above.
 *
 * It moved there when a THIRD main-app spec (`pipeline-editor.visual.spec.ts`)
 * needed the identical guard: that file cannot import it from here, because
 * importing a `*.spec.ts` re-runs this file's `test()` registrations inside the
 * importer and every route below would be snapshotted twice under two names.
 * Copying it would have left two definitions of the same guarantee free to
 * drift — the exact thing `lib/settle.ts`'s own header exists to prevent for
 * `settle()` and the base mask list. The measurement that chose the two halves
 * of that landmark moved with it, unchanged.
 */

interface VisualRoute {
  readonly name: string;
  readonly path: string;
  /** A landmark a loading state cannot render. Waited for before snapshotting. */
  readonly landmark: (page: Page) => ReturnType<Page['locator']>;
  /** Also capture a light-scheme variant. */
  readonly light?: boolean;
  /**
   * Photograph this route with a DIFFERENT project selected.
   *
   * Every route here otherwise uses the persona's own project, which is right:
   * a shot should show what the user who signed in sees. The exception is a
   * fixture that cannot live in the shared project — the seeded wiki is in one
   * of its own, because a permanent toolkit in project 1 makes J17.1's
   * empty-list premise unreachable (#519's lesson).
   *
   * Without this the shot would photograph a project with no wiki, whose
   * "No wiki has been generated for this project yet." is the SAME screen a
   * stalled listing shows — a baseline that records the failure it was added
   * to catch.
   */
  readonly project?: { readonly id: string; readonly name: string };
  /**
   * An interaction the shot needs after the shell has settled — opening a
   * drawer, toggling a panel. Runs before the landmark is awaited, so the
   * landmark can be what the interaction reveals.
   */
  readonly prepare?: (page: Page) => Promise<void>;
}

/*
 * `selectProject` lives in `lib/settle.ts` (ADR-0024 WP6 moved it: the
 * second-pack shots in `brand.visual.spec.ts` photograph the same seeded wiki).
 */

/*
 * Each entry carries an `@covers <route>` annotation naming the
 * `parity/screenshot-index.json` route it claims, verbatim.
 * `scripts/check-visual-coverage.mjs` counts ONLY exact matches.
 *
 * That gate first inferred coverage from navigation, and it lied: matching a
 * route's static prefix meant `/chat/:conversationId` counted as covered because
 * a spec navigated to `/app/chat`. It reported 15/15 wired shots while this very
 * file documented those four shots as uncovered. Declared coverage cannot
 * over-report.
 */

const ROUTES: readonly VisualRoute[] = [
  {
    // @covers /chat
    name: 'chat-empty-state',
    path: '/app/chat',
    // CHANGED, and this is a real defect fix rather than a tidy-up. The
    // previous landmark was `getByTestId('chat-input').or(getByRole(
    // 'textbox')).first()`, audited under #174 and KEPT on the strength of a
    // blanket-stall measurement. Re-measured with the shell allowed through
    // and the stall held for five minutes, `chat-input` resolves in BOTH
    // states — the composer is chrome, not data — so it could not have
    // distinguished a loaded chat page from a loading one.
    //
    // And there is something to distinguish, measured: with this route's API
    // stalled the conversation sidebar (mounted by #128) renders a row
    // SKELETON and the model chip reads "NONE"; loaded, the sidebar reads
    // "Still no conversations created." and the chip reads the seeded model
    // name. Sampled at 3/8/15/22/30/40s under a sustained stall, neither ever
    // converged — chip stayed "NONE", the empty-state text never appeared.
    //
    // The conversation empty state is the landmark: loaded YES, stalled no.
    landmark: (page) => page.getByText('Still no conversations created.'),
    light: true,
  },
  {
    // @covers /settings/personalization
    name: 'settings-personalization',
    path: '/app/settings/personalization',
    // The resolved profile block, NOT `getByRole('textbox').first()`.
    //
    // Found by the audit #174 asked for. This page's Formik form renders every
    // one of its textboxes while `useGetCurrentAuthor` is still in flight
    // (initialised from `undefined` author data); only `ProfileUserInfo`
    // switches between a Skeleton and the real name/avatar/email.
    //
    // Re-verified here under the corrected method. This page's own data IS
    // `/social/author`, which the shell allowlist normally lets through, so a
    // shell-allowed stall leaves the landmark resolving — that is the harness,
    // not the landmark. Stalling `/social/author` too: loaded YES, stalled no.
    landmark: (page) => page.getByTestId('profile-user-info'),
    light: true,
  },
  {
    // @covers /settings/create-personal-token
    name: 'settings-create-personal-token',
    path: '/app/settings/create-personal-token',
    // Audited under #174 and KEPT. This locator DOES resolve with the API
    // stalled — but that is not a defect here, because the screen has no
    // server data to wait for. It is a static react-hook-form: its only query,
    // `useListTokensQuery`, is passed `{ enabled: false }`, and every pixel of
    // its content area is client-rendered.
    //
    // Confirmed independently of any landmark, by the stronger test: screenshot
    // the route with its own API stalled and screenshot it loaded, and compare.
    // The two PNGs are BYTE-IDENTICAL, so there is no loading state for a
    // snapshot to photograph. (Two consecutive loaded runs are byte-identical
    // too, which is the determinism half.)
    //
    // The shell exposure that used to be recorded here as residual is no longer
    // residual — `shellSettled()` covers it for every route in this file.
    landmark: (page) => page.getByRole('textbox').first(),
  },
  {
    // @covers /settings/analytics
    name: 'settings-analytics',
    path: '/app/settings/analytics',
    // The KPI row, NOT `getByRole('main')`. This screen fetches its data after
    // mount and shows a spinner meanwhile; `main` (and `#root > *`) is present
    // during the spinner, so the landmark was satisfied instantly, `settle()`'s
    // 300ms elapsed, and the committed baseline was a photograph of the
    // spinner — the precise failure this file's header warns about, sitting in
    // the suite from the day it was written and only visible once the job was
    // allowed to run (issue #159).
    //
    // Re-measured under the corrected method: loaded YES, stalled no.
    //
    // The landmark has moved twice with the backend, and BOTH moves were
    // landmark changes rather than pixel changes — with a landmark that never
    // resolves, the test never reaches `toHaveScreenshot`, so
    // `--update-snapshots` writes no PNG and a regeneration run produces
    // nothing to review.
    //
    //   1. The analytics routes stopped fabricating zeros (#303), the endpoint
    //      answered 500, and the KPI row stopped rendering — so the landmark
    //      became the error branch's text.
    //   2. The gateway request log (shared migration 0099) gave the endpoint a
    //      real producer, it answers 200 again, and the KPI row is back.
    //
    // So the landmark is the KPI row's testid once more. It keeps the property
    // the comment above is defending — it is NOT satisfied during the spinner,
    // only once the query has resolved — where `getByRole('main')` would be,
    // which is how this baseline came to be a photograph of a spinner in the
    // first place.
    //
    // THE BASELINE PNG MUST BE REGENERATED with this change: the screen goes
    // from one line of error text to a populated KPI row and charts.
    landmark: (page) => page.getByTestId('analytics-kpi-row'),
  },
  {
    // @covers /settings/project-params
    name: 'settings-project-params',
    path: '/app/settings/project-params',
    // The resolved body, NOT `getByRole('main').or('#root > *')` — that was the
    // same landmark that turned the settings-analytics baseline into a
    // photograph of a spinner (#159). `ProjectContext` renders a bare
    // <CircularProgress> while `useGetProjectContext` is in flight, and both
    // `main` and `#root > *` are already in the tree at that moment.
    //
    // `project-context-body` is rendered only from a RESOLVED query: the
    // loading branch returns the spinner, the error branch returns a banner,
    // and the no-view-permission branch returns a different banner still.
    // Re-measured under the corrected method: loaded YES, stalled no.
    landmark: (page) => page.getByTestId('project-context-body'),
  },

  // ── Routes re-classified to `wired` in this change ────────────────────────
  // Each was `ready`/`blocked-codegen`/`hybrid-defect`/`needs-route-state` in
  // screenshot-index.json. That vocabulary is `route-wiring-map.json`'s Phase-0
  // status, recorded when 38 route files rendered `RouteShell` scaffolding
  // instead of the page components built for them; `ready` meant "the page
  // exists, the ROUTE does not render it". The wiring plan has since run, the
  // map now reports 50 routes wired and its own `--check` passes, and the
  // screenshot index was never updated to match. Every route below was
  // additionally opened against the running stack and confirmed to render its
  // real page, not scaffolding.

  {
    // @covers /agents/:tab
    name: 'agents-list-empty',
    path: '/app/agents/latest',
    // The empty state, NOT `getByTestId('agents-tab-all')`. Measured: the tab
    // strip renders during load (loaded YES, stalled YES) — it is chrome. The
    // empty-state copy renders only from a resolved, empty list: loaded YES,
    // stalled no.
    landmark: (page) => page.getByText('No agents yet'),
    light: true,
  },
  {
    // @covers /agents/create
    name: 'agent-create-form',
    path: '/app/agents/create',
    // No landmark can discriminate here, and measurement says none needs to.
    // The form is client-rendered; its one server input is `useListTags`, whose
    // result is empty in this tenant. Proven by the same byte-comparison used
    // for settings-create-personal-token: stalled and loaded screenshots are
    // BYTE-IDENTICAL, and two loaded runs are byte-identical to each other, so
    // there is no loading state to photograph and no run-to-run drift.
    // `agent-name-input` is asserted as a "the form mounted" guard, not as a
    // data landmark, and is labelled as such deliberately.
    landmark: (page) => page.getByTestId('agent-name-input'),
    light: true,
  },
  {
    // @covers /pipelines/:tab
    name: 'pipelines-list-empty',
    path: '/app/pipelines/latest',
    // Loaded YES, stalled no.
    landmark: (page) => page.getByText('No pipelines yet'),
  },
  {
    // @covers /pipelines/create
    name: 'pipeline-create-form',
    path: '/app/pipelines/create',
    // Same class as agent-create-form: stalled and loaded screenshots are
    // byte-identical, so there is no loading state. Guard locator only.
    landmark: (page) => page.getByTestId('create-pipeline-form-panel'),
  },
  {
    // @covers /skills/:tab
    name: 'skills-list-empty',
    path: '/app/skills/latest',
    // Loaded YES, stalled no.
    landmark: (page) => page.getByText('No skills yet'),
  },
  {
    // @covers /artifacts
    name: 'artifacts-empty',
    path: '/app/artifacts',
    // "No buckets found" (the resolved bucket LIST is empty), NOT "No buckets
    // created yet" — the two strings sit on the same screen and only the first
    // discriminates. Measured: "No buckets found" loaded YES / stalled no;
    // "No buckets created yet" loaded YES / stalled YES.
    //
    // The index note on this shot — "PR #82's stub renders a heading + one
    // button instead" — is stale: 46a90914 wired the route to
    // `pages/artifacts/Artifacts`, and the screen now renders the bucket
    // sidebar and the file pane.
    landmark: (page) => page.getByText('No buckets found'),
    light: true,
  },
  {
    // @covers /settings/users
    name: 'settings-users',
    path: '/app/settings/users',
    // A seeded row's email — DataGrid content, so it exists only once the user
    // list resolves. Loaded YES, stalled no (the pager text "Showing 1…"
    // behaves identically, either would do).
    landmark: (page) => page.getByText('e2e-member@autotest.local'),
    light: true,
  },
  {
    // @covers /credentials/:tab
    name: 'credentials-list-empty',
    path: '/app/credentials/latest',
    // Loaded YES, stalled no.
    landmark: (page) => page.getByText('No credentials yet'),
  },
  {
    // @covers /settings/secrets
    name: 'settings-secrets',
    path: '/app/settings/secrets',
    // The table's column header. Under a sustained stall this screen renders a
    // row SKELETON with no header, no "No secrets" and no pager; loaded, all
    // three appear. Measured: header/`No secrets`/`Rows per page`/`Page 1 of 1`
    // are all loaded YES, stalled no.
    //
    // This route is where the blanket-stall flaw in the first harness was
    // caught — see method note 1 in this file's header.
    //
    // The index marks this shot `hybrid-defect` for a reason unrelated to
    // rendering: the header Search is stubbed to `onSearchChange={() => {}}`.
    // That is a behaviour gap, not scaffolding, and it does not change what the
    // screen draws.
    landmark: (page) => page.getByRole('columnheader', { name: 'Name' }),
  },
  {
    // @covers /settings/model-configuration
    name: 'settings-model-configuration',
    path: '/app/settings/model-configuration',
    // MOVED, because the element it named no longer exists. The landmark was
    // the `Configurations` HEADING that `ConfigurationsPanel` drew above its
    // own toolbar. The settings parity work (fedca78b) deleted it: production
    // shows one title bar per settings tab, and this page had two — the
    // page's `DrawerPageHeader` ("AI Providers") and the panel's own heading
    // and rule under it. The panel row is now a right-aligned toolbar with no
    // heading at all, so this locator matched nothing and the shot was never
    // taken (`element(s) not found`, run 34016199152). A landmark that names
    // deleted chrome fails LOUDLY, which is the good half of this class — the
    // dangerous half is a landmark that survives a restructure by matching
    // something a loading state also renders.
    //
    // The LLMs section accordion replaces it, and it is a strictly stronger
    // landmark than the heading was:
    //   - `useConfigurationsBySection` returns `data: null` while ANY of its
    //     seven section queries is in flight, and `AIConfiguration` renders a
    //     literal "Loading…" in place of the panel for a null `data`. So no
    //     part of the panel exists during load.
    //   - `ConfigurationSection` returns its own "Loading…" line while
    //     `isLoading`, and `null` for an empty list. The accordion — and this
    //     testid with it — is reachable only from a RESOLVED, NON-EMPTY LLM
    //     list. The seed guarantees one (`scripts/e2e-stack.sh`'s
    //     `e2e-mock-model-llm` row, `section = 'llm'` in `p_1.configuration`).
    //
    // Re-measured under this file's own method rather than admitted on that
    // reading: `**\/api\/v2\/configurations\/**` stalled for five minutes with
    // the shell's four endpoints let through, sampled at 3/8/15/22/30/45/60s.
    // The testid was absent at every sample and "Loading…" present at every
    // sample; loaded, the testid is there and "Loading…" is gone. Loaded YES,
    // stalled no — the same fact from both directions, as before.
    //
    // NOT the section TITLE text ("LLMs"), even though it measures the same
    // today. `ConfigurationSection` prints that title from TWO branches — the
    // resolved accordion and its own "LLMs / Loading…" line — and only the
    // hook's current shape (`data` is null whenever anything is in flight)
    // keeps the second one unreachable from this page. The testid is on the
    // resolved branch alone, so it does not depend on that coincidence.
    //
    // THE BASELINE PNG MUST BE REGENERATED with this change: the page is the
    // rebuilt one (page header, pill-wrapped labelled selects, accordions).
    landmark: (page) => page.getByTestId('ai-providers-section-llms'),
  },
  {
    // @covers /toolkits/create
    name: 'toolkit-create-type-chooser',
    path: '/app/toolkits/create',
    // A toolkit type name from the fetched type list. "Choose the toolkit type"
    // is the static heading and renders during load (loaded YES, stalled YES);
    // `GitHub` and `Jira` are loaded YES / stalled no.
    landmark: (page) => page.getByText('GitHub'),
  },
  {
    // @covers /mcps/create
    name: 'mcp-create-type-chooser',
    path: '/app/mcps/create',
    // The resolved-and-empty local MCP list. "Choose the MCP type" is static
    // (loaded YES, stalled YES); this copy is loaded YES / stalled no.
    landmark: (page) => page.getByText('Still no local MCP available'),
  },
  {
    // @covers /agents-hub
    name: 'agents-hub',
    path: '/app/agents-hub',
    // A category chip that comes from the API. The page's obvious landmarks all
    // fail the experiment: "Welcome to Agent HUB", the search box, "No agents
    // found", and the chips `Trending`/`My Liked` are all present during load
    // (loaded YES, stalled YES) — `Trending` and `My Liked` are hardcoded, the
    // other seven chips are fetched. So the stalled screen is a plausible-
    // looking hub with two chips instead of nine, which is exactly the kind of
    // screenshot this suite must not accept. `Business Analyst` is one of the
    // fetched chips: loaded YES, stalled no.
    landmark: (page) => page.getByText('Business Analyst'),
    light: true,
  },
  {
    // @covers /deepwiki
    // @covers /deepwiki/$toolkitId
    name: 'deepwiki-browser',
    // The toolkit page itself: project 90200 seeds TWO wiki toolkits (the
    // read-only one and the one the generation journeys mutate), so
    // /app/deepwiki renders the chooser, not a wiki.
    path: '/app/deepwiki/9001',
    // The wiki's own project, not the persona's. The seeded wiki cannot live in
    // the shared project 1 — a permanent toolkit there makes J17.1's empty-list
    // premise unreachable — so this shot switches to the project the fixture is
    // in. Without it the baseline photographs a project with no wiki toolkit,
    // and `selectProject` below returns early with the default name.
    project: { id: '90200', name: 'e2e-deepwiki' },
    // THE SEEDED WIKI'S TITLE, which lives only in a manifest object in the
    // artifact store. Every other candidate on this screen fails the rule:
    // the page has no static heading of its own, the toolkit chooser does not
    // render for a project with one toolkit, and the empty state
    // ("No wiki has been generated for this project yet.") is what a STALLED
    // listing shows as well as an empty one — a baseline photographed against
    // it would record the screen this feature spent a phase not shipping.
    //
    // The title comes from two chained reads: the bucket listing, then the
    // manifest itself. Neither can resolve under a stall, and neither can be
    // produced by a screen that is merely mounted.
    // Measured: loaded YES, stalled no.
    landmark: (page) => page.getByText('E2E Service Wiki'),
    light: true,
  },
  {
    // @covers /deepwiki
    // The toolkit settings panel, open: the JSON draft editor and its Save.
    name: 'deepwiki-settings',
    // The toolkit page itself: project 90200 seeds TWO wiki toolkits (the
    // read-only one and the one the generation journeys mutate), so
    // /app/deepwiki renders the chooser, not a wiki.
    path: '/app/deepwiki/9001',
    project: { id: '90200', name: 'e2e-deepwiki' },
    prepare: async (page) => {
      await page.getByTestId('wiki-settings-toggle').click();
    },
    landmark: (page) => page.getByTestId('wiki-settings-panel'),
  },
  {
    // @covers /deepwiki
    // The wiki chat drawer, open and empty: the composer, the mode switch,
    // the invitation. Nothing has been asked, so nothing here is volatile.
    name: 'deepwiki-chat',
    // The toolkit page itself: project 90200 seeds TWO wiki toolkits (the
    // read-only one and the one the generation journeys mutate), so
    // /app/deepwiki renders the chooser, not a wiki.
    path: '/app/deepwiki/9001',
    project: { id: '90200', name: 'e2e-deepwiki' },
    prepare: async (page) => {
      await page.getByRole('button', { name: 'Ask about this repository' }).click();
    },
    landmark: (page) => page.getByTestId('wiki-chat-drawer'),
  },
];

for (const route of ROUTES) {
  test(`@visual ${route.name}`, async ({ page }) => {
    const projectName = await selectProject(page, route.project);
    await page.goto(BASE_URL + route.path, { waitUntil: 'domcontentloaded' });
    await shellSettled(page, projectName);
    if (route.prepare) await route.prepare(page);
    await expect(route.landmark(page).first()).toBeVisible({ timeout: 20_000 });
    await settle(page);

    await expect(page).toHaveScreenshot(`${route.name}.png`, {
      fullPage: false,
      mask: volatileRegions(page),
      ...SNAPSHOT_TOLERANCE,
    });
  });
}

/**
 * Switches the app to the light colour scheme through the REAL control, then
 * proves the switch took effect before the caller navigates anywhere.
 *
 * Not by writing `localStorage['el-mode']` and not by forcing CSS: issue #61 is
 * explicit that the light-scheme shots must exercise the app's own theme
 * mechanism, and journey 29 already establishes where that control lives
 * (`settings/personalization` renders `shared/ui/ThemeModeToggle`, a
 * ToggleButtonGroup with `Dark`/`Light` buttons). Faking the attribute would
 * make these six shots test the stylesheet while claiming to test the toggle.
 *
 * `data-el-scheme` (`shared/brand/constants.ts`'s `COLOR_SCHEME_ATTRIBUTE`,
 * whose own doc comment names this suite as an intended consumer) is what MUI's
 * `colorSchemeSelector` resolves to, so polling it is polling the thing the
 * stylesheet actually keys off — not a proxy for it.
 */
async function useLightScheme(page: Page): Promise<void> {
  await page.goto(BASE_URL + '/app/settings/personalization', { waitUntil: 'domcontentloaded' });
  const light = page.getByRole('button', { name: 'Light', exact: true });
  await expect(light).toBeVisible({ timeout: 20_000 });
  await light.click();
  await expect(light).toHaveAttribute('aria-pressed', 'true');
  await expect
    .poll(() => page.evaluate(() => document.documentElement.getAttribute('data-el-scheme')), {
      timeout: 10_000,
    })
    .toBe('light');
}

/*
 * Light-scheme variants.
 *
 * The index carries a light counterpart for most screens and every spec in this
 * suite captured the compiled default (`DEFAULT_COLOR_SCHEME = 'dark'`) only,
 * so the entire light half of the design — which is not a recolour but a
 * different accent hue, magenta against dark's cyan/teal per the index's
 * `accentHue` finding — had no reference at all.
 *
 * The landmark discipline is unchanged: the scheme is switched first, then the
 * route is loaded and waited for exactly as in the dark pass. The `@covers`
 * annotations are deliberately NOT repeated here — the coverage gate counts a
 * route once, and claiming it twice would inflate nothing but would misrepresent
 * these as additional route coverage rather than additional scheme coverage.
 */
for (const route of ROUTES.filter((r) => r.light)) {
  test(`@visual ${route.name}-light`, async ({ page }) => {
    await useLightScheme(page);
    const projectName = await selectProject(page, route.project);

    await page.goto(BASE_URL + route.path, { waitUntil: 'domcontentloaded' });
    await shellSettled(page, projectName);
    if (route.prepare) await route.prepare(page);
    await expect(route.landmark(page).first()).toBeVisible({ timeout: 20_000 });
    // The scheme must still be light on the route under test — a navigation
    // that reset it would otherwise produce a second dark baseline under a
    // light name, and it would look correct.
    expect(await page.evaluate(() => document.documentElement.getAttribute('data-el-scheme'))).toBe('light');
    await settle(page);

    await expect(page).toHaveScreenshot(`${route.name}-light.png`, {
      fullPage: false,
      mask: volatileRegions(page),
      ...SNAPSHOT_TOLERANCE,
    });
  });
}

/*
 * Rail-collapsed variants.
 *
 * `sidebarCollapsed.store` is backed by `localStorage['sidebar.collapsed']`
 * (`widgets/sidebar/lib/collapsedPersistence.ts`), so a spec that assumed the
 * rail was expanded would depend on whatever the previously-run spec left
 * behind. Each variant here sets the state explicitly through the real toggle
 * and asserts the resulting width before snapshotting, so it neither inherits
 * nor leaks state.
 */
test('@visual chat-empty-rail-collapsed', async ({ page }) => {
  await page.goto(BASE_URL + '/app/chat', { waitUntil: 'domcontentloaded' });
  await shellSettled(page);
  await expect(page.getByText('Still no conversations created.')).toBeVisible({ timeout: 20_000 });

  // The real control (`SidebarHeader`'s `sidebar-toggle`), not a store poke.
  await page.getByTestId('sidebar-toggle').click();
  // Collapsed is proven by the project-name block disappearing —
  // `ProjectSwitcher` renders its `Project:` / name column behind
  // `{!collapsed && …}`, so this is the inverse of the assertion
  // `shellSettled()` just made, on the same element.
  //
  // NOT the nav label: `SidebarNavItem` does render icon-only when collapsed
  // (`showLabel={!collapsed}`), but the `<a>` keeps `aria-label="Credentials"`,
  // so `getByRole('link', { name: 'Credentials' })` stays matched and visible.
  // Measured, not assumed — that was this spec's first draft and it failed all
  // three attempts with the locator resolving 24 times to the collapsed link.
  await expect(page.locator('button').filter({ hasText: 'Default Project' }).first()).toBeHidden({
    timeout: 10_000,
  });
  await settle(page);

  await expect(page).toHaveScreenshot('chat-empty-rail-collapsed.png', {
    fullPage: false,
    mask: volatileRegions(page),
    ...SNAPSHOT_TOLERANCE,
  });
});

/*
 * NOT COVERED, and why — keep this current, it is the honest half of the suite:
 *
 *  - `/chat/:conversationId` (4 shots). The obstacle is determinism, not a
 *    missing backend: POST /api/v2/elitea_core/conversations/prompt_lib/1
 *    returns 201 and persists a row. Seeding a conversation on each run adds a
 *    row to the sidebar list, so consecutive runs render different lists and the
 *    snapshot diffs for reasons that have nothing to do with the UI. Fixing it
 *    means seeding to a known list state. Recorded as an EXEMPT entry in
 *    scripts/check-visual-coverage.mjs, which prints it on every run.
 *
 *    Now more tractable than when it was written, and worth saying why: the
 *    conversation sidebar is actually mounted (#128), and the `/chat` landmark
 *    above is proof it renders from resolved data ("Still no conversations
 *    created." is the empty branch of that very list). A seed that creates a
 *    FIXED set of conversations before the visual project runs would make the
 *    list deterministic and unblock all four shots.
 *
 *  - `/help-center` (2 shots). Still EXEMPT, but no longer for the reason
 *    written here before unit A14. `useResourcesConfig` now HAS a backend: the
 *    admin Configuration port (#200) made
 *    `GET /admin/plugin_config_values/prompt_lib/resources` serve the section
 *    an administrator edits, and the hook reads it — which is what issue #26
 *    was waiting for, and journey 36g asserts end to end. What remains is a
 *    SEEDING question rather than a wiring one: this stack configures no
 *    resource links, so the cards still render "No links configured" and a
 *    baseline would still photograph an unconfigured screen. Seeding a fixed
 *    set of links (as the conversation-list note above proposes for `/chat`)
 *    is now all that stands between this route and two real shots.
 *    (Measured before: stalled and loaded screenshots were byte-identical, so
 *    it will not need a data landmark, only a guard.)
 *
 *  - `/onboarding` (2 shots). Left at `ready`, not re-classified. The route
 *    renders a real page, but what it renders ("Welcome to Elitea!" + a single
 *    "SURE, LET'S GO!" CTA) is not the screen the index describes (a 48-slide
 *    tour with a "1 / 48" counter and a gradient CTA pill). That is a parity
 *    question, not a wiring one, and answering it is not this change's job — so
 *    it keeps its status and gets no baseline. It was NOT put through the stall
 *    experiment; nothing here claims it is or is not snapshottable.
 *
 *  - `/agents/:tab/:agentId`, `/pipelines/:tab/:agentId`,
 *    `/toolkits/create/:toolkitType`, `/credentials/:tab/:credential_uid`
 *    (7 shots). Detail/editor routes that need a seeded entity to reach with
 *    real data. Same class as `/chat/:conversationId`: they need a fixed seed,
 *    not a landmark. Left at their current status — none was measured.
 *
 *  - `/user-public/*` (14 shots). Needs an author with published content; the
 *    seeded tenant has none. Left at `needs-route-state`, which is accurate.
 *
 *  - Light-scheme variants for the routes not marked `light` above. The six the
 *    index carries are covered; the rest would be more baselines without more
 *    theme surface, and PNGs do not delta-compress.
 */
