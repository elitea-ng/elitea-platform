/**
 * Journey 17: Create toolkit → configure → test connection (JRNY-017)
 *
 * Spec §8.5 acceptance (from parity/manifest/toolkits.json JRNY-017).
 * Acceptance: the test result is displayed; the saved toolkit appears in the list.
 *
 * ── NOT COVERED (product gaps — deliberately NOT asserted around) ───────────
 *
 * 1. "Test connection" on the toolkit FORM — still no UI. `onTestConnection` is
 *    only a prop TYPE
 *    (features/toolkits/ui/form/ToolkitForm/ToolkitsOperationButtons.types.ts:69);
 *    ToolkitsOperationButtons never destructures or renders it.
 *
 *    CORRECTED: the second half of this note said the live test PANE was a
 *    composition gap and that "the test result is displayed" could not be
 *    asserted. Both halves are now false. `TestToolPane`
 *    (features/toolkits/ui/test-tools/TestToolPane.tsx) is the real pane, over
 *    the synchronous run route POST /elitea_core/test_tool/prompt_lib/{p}/{t},
 *    and `toolkits.test-pane.spec.ts` (J17b) drives it: pick a tool, fill it,
 *    Run, read the outcome. That file is where the missing half of JRNY-017
 *    lives now.
 *
 * 2. CLOSED. Edit-and-save on the detail page used to have no save affordance
 *    at all: ToolkitsOperationButtons draws no persistent button, its update
 *    path fires only on eventEmitter ToolEvents.ToolkitsUpdateToolkit, and
 *    nothing in the app emitted it. The page header emits it now, and J17.8
 *    below is the journey this note asked for.
 *
 * ── FORMERLY KNOWN-FAILING — BOTH DEFECTS ARE FIXED (#129), re-measured on the
 *    real E2E stack on 2026-08-09. Kept as history because the assertions below
 *    are shaped by them:
 *
 * A. GET /elitea_core/toolkits/prompt_lib/{projectID} used to be routed to
 *    toolkitHandler.List (the toolkit-INSTANCE list, {rows,total}) instead of
 *    ListTypeSchemas, so ToolkitTypeSelector iterated the pagination envelope
 *    and rendered label-less "rows"/"total" tiles. It now serves the real
 *    type→schema map — measured keys: application, artifact, custom, database,
 *    datasource, github, jira, openapi. That is what J17.2 asserts through the
 *    "GitHub" tile, and what makes J17.3 render ToolBase rather than
 *    ToolCustom (see J17.3's own header).
 *
 * B. POST /elitea_core/tools/prompt_lib/{projectID} used to 500 on an
 *    `owner_id NOT NULL` violation in pgRepo.CreateToolkit. It now answers 201.
 */
import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, API_BASE, DEFAULT_PROJECT_ID, clickCreateButton } from '../../fixtures/api';
import { readsPlatformFlags } from '../../fixtures/platformFlags';

/** Unique to THIS file so concurrent journeys never collide on a name. */
const toolkitName = (): string => `${AUTOTEST_PREFIX}tk${Date.now()}`;

/*
 * J17.3 asserts the "Make tools available by MCP" field, which `ToolBase` draws
 * only while `useIsMcpVisible()` is true — that is the platform-wide
 * `mcp_enabled` row, and `admin.features.spec.ts` turns it off and back on to
 * prove the platform obeys it. J17.3 failed inside that window in 2 local runs
 * of 10 and in three CI runs of `E2E (webkit)` (issue #519). The shared half of
 * the platform-flag lock keeps this file out of the window.
 */
readsPlatformFlags(test);

/** Ids created by this file, deleted in afterAll. Never a blanket `autotest_` sweep — other specs run concurrently. */
const createdIds: string[] = [];

test.afterAll(async ({ browser }) => {
  if (createdIds.length === 0) return;
  const ctx = await browser.newContext();
  for (const id of createdIds) {
    await ctx.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`);
  }
  await ctx.close();
});

/**
 * The create page's own discriminating landmark: ToolkitTypeSelector's
 * CategoryFilter search box. A stub route with a bare heading has no form
 * control; the placeholder text comes from
 * toolkits.toolkitTypeSelector.searchToolkit.
 */
function typeSearchBox(page: Page) {
  return page.getByPlaceholder('Search toolkits');
}

/*
 * J17.1 — "an empty toolkit list redirects to the create page" — LIVES IN
 * `toolkits.emptyList.spec.ts`, and the move is the fix for its failures, not
 * tidying.
 *
 * Its subject is the redirect, and the redirect's input is the SERVER's row
 * count for the shared project. This file, `toolkits.catalogue.spec.ts` and
 * the MCP journeys all create toolkits in that project, and `fullyParallel`
 * runs them together — J17C.1 alone holds one toolkit per served category
 * (eight or more) for the length of an eleven-minute test. Measured on this
 * tree: `expect(received).toBe(0)` received 8.
 *
 * A lock cannot fix that, and the poll below could not either: there is no
 * window in the run where the shared project is empty. What makes the
 * precondition true is ORDER — the journey now runs in its own Playwright
 * project, which every engine project depends on, so it is judged before the
 * first sibling has created anything.
 */

test('J17.2: the create page offers real, server-supplied toolkit types', async ({ page }) => {
  // Budget, not behaviour: this test's own waits (two 15 s tile waits, seven
  // further assertions on default timeouts, and `checkA11y` over a grid of
  // sixty-odd tiles) already exceed the 30 s file default, so it passed only
  // while every step was fast. In the first CI read of the ported suite it
  // timed out on webkit INSIDE `checkA11y` — axe injecting and analysing this
  // page, which is the largest DOM in the toolkits area, not a missing tile.
  test.setTimeout(90_000);

  // PASSES as of the #129 route fix. The tile label "GitHub" is derivable ONLY
  // from a schema map that actually contains the key `github`
  // (entities/toolkit/model/toolMenu.ts labels each map key through ToolTypes),
  // so this cannot be satisfied by a stub or by the pagination envelope the
  // endpoint used to return.
  await page.goto(BASE_URL + '/app/toolkits/create');
  await expect(typeSearchBox(page)).toBeVisible({ timeout: 15_000 });

  await expect(page.getByRole('button', { name: 'GitHub', exact: true })).toBeVisible({ timeout: 15_000 });

  // The catalogue is the SDK's, not a hand-written map of eight keys. These
  // three tiles exist only because the server projects the pinned SDK snapshot,
  // and each comes from a different category, so a catalogue that collapsed
  // back to one group fails here as well.
  await expect(page.getByRole('button', { name: 'Confluence', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'SharePoint', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'QTest', exact: true })).toBeVisible();
  // And the headings the metadata groups them under.
  await expect(page.getByText('Code Repositories', { exact: true }).first()).toBeVisible();
  await expect(page.getByText('Test Management', { exact: true }).first()).toBeVisible();
  // AWS is served, and served HIDDEN: the admitted Python worker image
  // cannot import it (current_python_worker_toolkit_capability_snapshot
  // .json's `unsupported_import_keys`), so offering the tile would produce a
  // toolkit that fails at its first tool call. Absence here is the
  // capability projection working. NOT Slack — #869 (same branch) pinned
  // slack_sdk on the Python image specifically so Slack import succeeds;
  // asserting it hidden here would be asserting the gap #869 just closed.
  // `aws` has no frontend ToolTypes entry, so its tile falls back to
  // `humanizeToolkitTypeKey` ("aws" -> "Aws" — see toolMenu.ts).
  await expect(page.getByRole('button', { name: 'Aws', exact: true })).toHaveCount(0);

  // checkA11y earns its place here and has now caught TWO different causes of the
  // same critical `button-name` violation, which is why it stays unconditional:
  //   1. before #129, the page iterated the {rows,total} pagination envelope and
  //      rendered a nameless tile per envelope KEY;
  //   2. after #129 served real types, `database` and `datasource` had no entry
  //      in the frontend ToolTypes map and the label fell back to '' — still two
  //      nameless tiles, entirely different cause.
  // Fixed by making the label fall back to a humanised key, so a backend type the
  // frontend has never heard of degrades to a readable name instead of nothing.
  //
  // (A dedicated nameless-button count was tried and REMOVED as vacuous:
  // Playwright's getByRole(..., {name: ''}) does not match empty accessible
  // names, so it asserted nothing.)
  await checkA11y(page);
});

/**
 * WHY THIS TEST NO LONGER EXPECTS TO FAIL, AND WHY IT NO LONGER TOUCHES
 * CODEMIRROR (measured, not inferred — all three on the real E2E stack):
 *
 *  1. `.cm-content` count on the create page after picking Custom is **0**.
 *     There is no CodeMirror element at all, so the old failure was neither a
 *     serialisation/spacing change nor CodeMirror virtualising text out of the
 *     DOM — both of those require the editor to exist.
 *  2. The type DOES reach the draft. Saving issues
 *     `POST /api/v2/elitea_core/tools/prompt_lib/1` with body
 *     `{"type":"custom","name":...,"settings":{"selected_tools":[]}}` and the
 *     server answers **201** (#129's owner_id 500 is fixed).
 *  3. The real cause is component selection, and it is CORRECT behaviour:
 *     `GET /elitea_core/toolkits/prompt_lib/{id}` now serves the real
 *     type→schema map, and its `custom` entry is
 *     `{"properties":{"selected_tools":{...}},"type":"object"}`. Because that
 *     schema has a truthy `.type`, `getToolComponent`
 *     (features/toolkits/lib/helpers/toolComponent.helpers.ts:81) resolves
 *     `ToolBase` — the structured form — and never mounts `ToolCustom`'s JSON
 *     editor. `ToolCustom` is the no-typed-schema fallback; back when the
 *     endpoint returned the `{rows,total}` pagination envelope the map had no
 *     `custom` key, the schema degraded to `{properties:{}}`, and the JSON
 *     editor is what the original test saw.
 *
 * So the CodeMirror assertions encoded a UI shape that only appears when the
 * backend is broken. They are replaced below by assertions against the form
 * the app actually renders — which is itself backend-derived: the Tools
 * section's "Make tools available by MCP" field is drawn by
 * `ToolBase.render.tsx:296-306`, reachable ONLY via the ToolBase branch, i.e.
 * only when the server supplied a typed `custom` schema.
 */
test('J17.3: create a toolkit, persist it, and reopen it from the list', async ({ page }) => {
  const name = toolkitName();

  await page.goto(BASE_URL + '/app/toolkits/all');
  await page.waitForURL(/\/app\/toolkits\/(all|create)/, { timeout: 20_000 });
  if (!/\/create/.test(page.url())) await clickCreateButton(page);
  await page.waitForURL(/\/app\/toolkits\/create/, { timeout: 20_000 });
  await expect(typeSearchBox(page)).toBeVisible({ timeout: 15_000 });

  // Pick the `custom` type — a genuine key in the server's own type→schema map
  // (GET /elitea_core/toolkits/prompt_lib/{id}).
  await page.getByRole('button', { name: 'Custom', exact: true }).click();

  // The selector was REPLACED by the real form: ToolBase's fields plus
  // CreateToolkitToolTabBar's Save. Neither exists on the selector screen.
  await expect(page.getByRole('button', { name: 'Save', exact: true })).toBeEnabled({ timeout: 15_000 });

  // Backend-derived, per the header: this field is rendered only down the
  // ToolBase branch, which `getToolComponent` picks only because the server's
  // `custom` schema carries `"type": "object"`. Against the old {rows,total}
  // envelope the app fell back to ToolCustom's JSON editor and this checkbox
  // did not exist.
  await expect(page.getByRole('checkbox', { name: 'Make tools available by MCP' })).toBeVisible({ timeout: 15_000 });

  // The form is seeded from the picked type's initial values, then renamed.
  const nameField = page.getByRole('textbox', { name: 'Toolkit Name' });
  await expect(nameField).toHaveValue('Custom tool', { timeout: 15_000 });
  await nameField.fill(name);

  const [createResp] = await Promise.all([
    page.waitForResponse(
      (r) => r.request().method() === 'POST' && /\/elitea_core\/tools\/prompt_lib\//.test(r.url()),
      { timeout: 20_000 },
    ),
    page.getByRole('button', { name: 'Save', exact: true }).click(),
  ]);
  // #129's owner_id NOT NULL 500 is fixed. Do not weaken this to `toBeLessThan(500)`.
  expect(createResp.status(), await createResp.text()).toBe(201);
  const created = (await createResp.json()) as { id: string };
  expect(created.id).toBeTruthy();
  createdIds.push(created.id);

  // CreateToolkit's only role=alert is "Failed to create the toolkit."
  await expect(page.getByRole('alert')).toHaveCount(0);

  await checkA11y(page);

  // ── Persistence: a FULL reload, so the 30 s-stale list cache cannot answer.
  await page.goto(BASE_URL + '/app/toolkits/all');
  const card = page
    .getByTestId('toolkits-list-panel')
    .getByTestId('toolkit-card')
    .filter({ hasText: name });
  await expect(card).toBeVisible({ timeout: 20_000 });

  await checkA11y(page);

  // ── Detail: opening the card must reach the id-bearing route and render
  // content fetched from GET /elitea_core/tool/prompt_lib/{id}/{toolkitId}.
  await card.click();
  // `(\?|$)` because the shell appends its own persisted query string.
  await expect(page).toHaveURL(new RegExp(`/app/toolkits/all/${created.id}(\\?|$)`), { timeout: 20_000 });
  // Route landmark unique to EditToolkit. toBeAttached, not toBeVisible — the
  // slot is an intentionally empty Box (composition gap 1 above).
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 20_000 });
  // Backend-derived: the detail fetch populated the form with the persisted
  // name. A stub route cannot produce this.
  await expect(page.getByRole('textbox', { name: 'Toolkit Name' })).toHaveValue(name, { timeout: 20_000 });

  await checkA11y(page);
});

/**
 * J17.5 — the Indexes tab (#149).
 *
 * The positive half of the pair whose negative half lives in
 * `../mcps/mcps.oauth.spec.ts` (an MCP screen must NOT offer this tab). Here
 * the tab MUST be offered, and must render the real
 * `features/toolkits/indexes` container rather than the empty
 * `<Box data-testid="edit-toolkit-indexes-tab-panel"/>` that stood in for it
 * until 2026-08-09.
 *
 * Why `artifact`. The tab is gated on the toolkit TYPE's schema offering at
 * least one `IndexesToolsEnum` tool (baseline `EditToolkit.jsx:210-216`).
 * Measured against this stack's own
 * `GET /elitea_core/toolkits/prompt_lib/{projectId}`, exactly two types
 * qualify — `artifact` and `datasource`, both via
 * `properties.selected_tools.args_schemas.index_data`. `github`, `jira`,
 * `database`, `application`, `custom` and `openapi` offer none, so on any of
 * those the tab is correctly absent and this journey would be asserting the
 * wrong thing. The type is therefore backend-DERIVED at runtime below, not
 * hardcoded on faith: if the schema ever stops offering `index_data` the
 * assertion fails loudly instead of silently testing nothing.
 */
test('J17.5: a toolkit whose type supports indexing renders the real Indexes panel', async ({ page }) => {
  /*
   * THE DEFAULT 30 s BUDGET CANNOT HOLD THIS TEST — measured, not guessed.
   *
   * It reads the catalogue, creates a toolkit over the API, opens the editor,
   * waits up to 20 s for the Indexes panel, waits for the panel's own list
   * request, and only THEN runs `checkA11y`, which walks the whole rendered
   * page in a separate axe pass. On a loaded runner that last pass is what
   * ran out of budget, and the failure read as `frame.evaluate: Test timeout`
   * inside `fixtures/axe.ts` — the shape of a budget exhausted earlier, not
   * of a broken scan (webkit, retried green).
   */
  test.setTimeout(90_000);

  // ── Derive the type from the live catalogue rather than trusting a literal.
  const schemasResp = await page.request.get(
    `${API_BASE}/elitea_core/toolkits/prompt_lib/${DEFAULT_PROJECT_ID}`,
  );
  expect(schemasResp.status(), await schemasResp.text()).toBe(200);
  const schemas = (await schemasResp.json()) as Record<
    string,
    { properties?: { selected_tools?: { args_schemas?: Record<string, unknown> } } }
  >;
  const indexingType = Object.entries(schemas).find(
    ([, schema]) => schema.properties?.selected_tools?.args_schemas?.['index_data'] !== undefined,
  )?.[0];
  expect(
    indexingType,
    `no toolkit type in GET /elitea_core/toolkits/prompt_lib offers index_data; measured types: ${Object.keys(schemas).join(', ')}`,
  ).toBeTruthy();

  const name = toolkitName();
  const createResp = await page.request.post(
    `${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`,
    {
      data: {
        name,
        type: indexingType,
        description: 'JRNY-017 indexes-tab fixture',
        settings: { selected_tools: ['index_data'] },
      },
    },
  );
  expect(createResp.status(), await createResp.text()).toBe(201);
  const created = (await createResp.json()) as { id: string };
  createdIds.push(created.id);

  // The index list request the mounted container issues. Registered BEFORE
  // navigation so the tab click cannot race it.
  const indexListRequest = page.waitForResponse(
    (r) => /\/elitea_core\/index_meta\/prompt_lib\//.test(r.url()),
    { timeout: 20_000 },
  );

  await page.goto(`${BASE_URL}/app/toolkits/all/${created.id}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 20_000 });

  // The tab is OFFERED here — the exact thing the MCP journey forbids.
  const indexesTab = page.getByRole('tab', { name: 'Indexes' });
  await expect(indexesTab).toBeVisible({ timeout: 20_000 });
  await indexesTab.click();

  const panel = page.getByTestId('edit-toolkit-indexes-tab-panel');
  await expect(panel).toBeVisible();

  /*
   * The assertions that discriminate a REAL container from the placeholder.
   * `toBeVisible()` on the panel alone did not — an empty Box satisfied it
   * for months. These three cannot be satisfied without
   * `IndexesContainer` -> `IndexesList` actually rendering:
   *   - the "Add index" control (`IndexesList.tsx:46`, its aria-label),
   *   - the empty-state copy for a toolkit with no indexes yet
   *     (`IndexesList.tsx:57`),
   *   - and a real network round trip to the index-meta endpoint, which only
   *     `useIndexesListQuery` issues.
   */
  await expect(panel.getByRole('button', { name: 'Add index' })).toBeVisible({ timeout: 20_000 });
  await expect(panel.getByText('Still no indexes created')).toBeVisible();

  const indexListResponse = await indexListRequest;
  expect(indexListResponse.status(), await indexListResponse.text()).toBe(200);
  expect(indexListResponse.url()).toContain(`/${created.id}`);

  await checkA11y(page);
});

/** `ToolActionsSelector.toOption`: a chip label is the tool name with the first letter capitalised and every underscore turned into a space. */
function toolChipLabel(toolName: string): string {
  return `${toolName.charAt(0).toUpperCase()}${toolName.slice(1)}`.replaceAll('_', ' ');
}

/**
 * #440. Both halves of the defect, at the surface that showed it.
 *
 * Issue #381 made the two tool-catalogue routes answer a failed database read
 * with an error. Before that a failed read gave 200 with an empty list, and
 * the screen still behaved that way: one empty tool picker stood for "this
 * toolkit offers no tools", "this toolkit publishes its tools at run time"
 * and "the read failed".
 *
 * The type and the tool name below are DERIVED from the live catalogue, never
 * hardcoded: if the server stops serving tools for that type the test fails
 * loudly instead of asserting nothing.
 */
test('J17.6: the Tools section lists the tools the server supplies for the toolkit type', async ({ page }) => {
  const schemasResp = await page.request.get(`${API_BASE}/elitea_core/toolkits/prompt_lib/${DEFAULT_PROJECT_ID}`);
  expect(schemasResp.status(), await schemasResp.text()).toBe(200);
  const schemas = (await schemasResp.json()) as Record<
    string,
    { properties?: { selected_tools?: { args_schemas?: Record<string, unknown>; items?: { enum?: string[] } } } }
  >;

  const withTools = Object.entries(schemas).find(([, schema]) => {
    const selected = schema.properties?.selected_tools;
    return Object.keys(selected?.args_schemas ?? {}).length > 0 || (selected?.items?.enum ?? []).length > 0;
  });
  expect(
    withTools,
    `no toolkit type in GET /elitea_core/toolkits/prompt_lib declares any tool; measured types: ${Object.keys(schemas).join(', ')}`,
  ).toBeTruthy();

  const [type, schema] = withTools as [string, { properties?: { selected_tools?: { args_schemas?: Record<string, unknown>; items?: { enum?: string[] } } } }];
  const selected = schema.properties?.selected_tools;
  const toolNames = Object.keys(selected?.args_schemas ?? {}).length > 0 ? Object.keys(selected?.args_schemas ?? {}) : (selected?.items?.enum ?? []);
  const firstTool = toolNames[0] as string;

  const created = await page.request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: { name: toolkitName(), type, description: 'JRNY-017 tool-picker fixture (#440)', settings: { selected_tools: [] } },
  });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  createdIds.push(id);

  await page.goto(`${BASE_URL}/app/toolkits/all/${id}`, { waitUntil: 'domcontentloaded' });

  // The chip is drawn from the SERVED schema, never from a list compiled into
  // the app, and the error state must not stand where a real list exists.
  await expect(page.getByRole('button', { name: toolChipLabel(firstTool), exact: true })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('toolkit-form-tool-list-error')).toHaveCount(0);
});

test('J17.7: a failed tool-catalogue read shows an error with a retry, not an empty Tools section', async ({ page }) => {
  const schemasResp = await page.request.get(`${API_BASE}/elitea_core/toolkits/prompt_lib/${DEFAULT_PROJECT_ID}`);
  expect(schemasResp.status(), await schemasResp.text()).toBe(200);
  const schemas = (await schemasResp.json()) as Record<
    string,
    { properties?: { selected_tools?: { args_schemas?: Record<string, unknown>; items?: { enum?: string[] } } } }
  >;

  // A type that declares no tools of its own publishes them at run time, so
  // the catalogue route is the tier that feeds its Tools section.
  const runtimeType = Object.entries(schemas).find(([, schema]) => {
    const selected = schema.properties?.selected_tools;
    return selected !== undefined && Object.keys(selected.args_schemas ?? {}).length === 0 && (selected.items?.enum ?? []).length === 0;
  })?.[0];
  expect(
    runtimeType,
    `every toolkit type declares its own tools, so no type exercises the catalogue tier; measured types: ${Object.keys(schemas).join(', ')}`,
  ).toBeTruthy();

  const created = await page.request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: { name: toolkitName(), type: runtimeType, description: 'JRNY-017 failed-read fixture (#440)', settings: { selected_tools: [] } },
  });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  createdIds.push(id);

  // The server answers this route with an error for a lost read (#381). The
  // route is failed at the network layer here because a healthy stack cannot
  // be asked to lose a read on demand.
  // Counted off the wire, not inside the route handler: a glob that matches
  // nothing leaves the handler silent, and a count taken there would then read
  // zero for two different reasons.
  const catalogueUrls: string[] = [];
  page.on('request', (request) => {
    if (/toolkit_(available|discover)_tools/.test(request.url())) catalogueUrls.push(request.url());
  });
  let catalogueReads = 0;
  let failReads = true;
  // A `*` glob, not an extglob: Playwright's URL matcher supports `*`, `**`,
  // `?`, `[]` and `{a,b}` only, so `@(available|discover)` matched nothing and
  // the real route answered 200 — the test then measured the healthy path
  // while claiming to measure the failed one.
  await page.route('**/elitea_core/toolkit_*_tools/**', async (route) => {
    catalogueReads += 1;
    if (!failReads) {
      await route.continue();
      return;
    }
    await route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ error: 'read available tools failed' }) });
  });

  await page.goto(`${BASE_URL}/app/toolkits/all/${id}`, { waitUntil: 'domcontentloaded' });

  // The read itself first: without it the error assertion below could pass or
  // fail for a reason that has nothing to do with the failed-read state.
  await expect
    .poll(() => catalogueUrls.length, { message: `the picker never read the catalogue for type ${String(runtimeType)}`, timeout: 20_000 })
    .toBeGreaterThan(0);
  expect(catalogueReads, `the catalogue read was not intercepted: ${catalogueUrls.join(', ')}`).toBeGreaterThan(0);

  const error = page.getByTestId('toolkit-form-tool-list-error');
  await expect(error, `no failed-read state for type ${String(runtimeType)}`).toBeVisible({ timeout: 20_000 });
  await expect(error).toContainText('The tool list did not load. Try again.');

  // The retry reads again, and the error goes when the read succeeds.
  const readsBeforeRetry = catalogueReads;
  failReads = false;
  await error.getByRole('button', { name: 'Retry' }).click();
  await expect(error).toHaveCount(0, { timeout: 20_000 });
  expect(catalogueReads).toBeGreaterThan(readsBeforeRetry);
});

/**
 * J17.8 — edit a saved toolkit from its own page and persist the change.
 *
 * The half of JRNY-017 that note 2 above recorded as impossible. It is the
 * whole point of an edit route, and it went unasserted for as long as the
 * route had no Save control: every field on the page was editable and no edit
 * could ever leave the browser.
 *
 * The proof is a SERVER read, not the screen: the form is what would keep
 * showing the typed text whether or not the write happened.
 */
test('J17.8: editing a toolkit’s description and pressing Save persists it', async ({ page }) => {
  test.setTimeout(90_000);

  const name = toolkitName();
  const created = await page.request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: { name, type: 'custom', description: 'JRNY-017 edit-and-save fixture', settings: { selected_tools: [] } },
  });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = (await created.json()) as { id: string };
  createdIds.push(id);

  const edited = `${AUTOTEST_PREFIX}edited_${Date.now()}`;

  await page.goto(`${BASE_URL}/app/toolkits/all/${id}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 20_000 });

  const saveButton = page.getByTestId('toolkit-save-button');
  // Dirty-gated, exactly like the agent editor: nothing has changed yet, so
  // there is nothing to save.
  await expect(saveButton, 'the toolkit edit page must offer a Save control').toBeVisible({ timeout: 20_000 });
  await expect(saveButton, 'an untouched toolkit has nothing to save').toBeDisabled();

  // `custom` declares no `toolkit_name`-flagged property, which is what makes
  // `NameDescriptionInput` draw the Name/Description pair (its own
  // `resolveDescriptionVisibility`). The same reasoning J17.3 relies on for the
  // Toolkit Name field it already asserts on this route.
  const description = page.getByRole('textbox', { name: 'Description' });
  await expect(description, 'the toolkit editor must offer the Description field this journey edits').toBeVisible({ timeout: 20_000 });
  await description.fill(edited);

  await expect(saveButton, 'an edit must make the toolkit saveable').toBeEnabled({ timeout: 20_000 });

  const [saveResponse] = await Promise.all([
    page.waitForResponse(
      (r) => r.request().method() === 'PUT' && /\/elitea_core\/tool\/prompt_lib\//.test(r.url()),
      { timeout: 30_000 },
    ),
    saveButton.click(),
  ]);
  expect(saveResponse.status(), await saveResponse.text()).toBeLessThan(300);

  // THE SERVER's own copy, read back through its own route. Polled, because
  // the write and this read are two round trips.
  await expect
    .poll(
      async () => {
        const read = await page.request.get(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`);
        if (!read.ok()) return `read answered ${read.status()}`;
        // Both shapes accepted on purpose: this route answers the row itself,
        // and a reader that assumed the wrong one of the two would report a
        // successful write as a lost one.
        const body = (await read.json()) as { description?: string; data?: { description?: string } };
        return body.description ?? body.data?.description ?? '';
      },
      { message: 'the edited description never reached the server', timeout: 30_000 },
    )
    .toBe(edited);

  // And the control settles back: a saved toolkit has nothing left to save.
  await expect(saveButton).toBeDisabled({ timeout: 20_000 });
});
