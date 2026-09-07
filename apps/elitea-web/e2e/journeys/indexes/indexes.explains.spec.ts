/**
 * JRNY-INDEX-A — the Indexes tab EXPLAINS when it cannot work.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS FILE EXISTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The 2026-09-06 production-parity walk graded "Indexes" unusable end to end
 * (validation matrix row F4). Three observations, and none of them was a
 * missing feature:
 *
 *   1. `GET /api/v2/elitea_core/index_meta/prompt_lib/2/1` answered
 *      `400 {"error":"PGVector configuration is missing for toolkit 1"}` — on
 *      a plain READ — and the rail said "Still no indexes created". The server
 *      had named the missing prerequisite in a sentence written for a human,
 *      and the client threw it away.
 *   2. The screen therefore looked like an empty-but-healthy feature while it
 *      was in fact unusable until a vector store exists.
 *   3. Nothing anywhere connected the two facts.
 *
 * `toolkits.lifecycle.spec.ts`'s J17.5 beside this folder already proves the
 * HAPPY read: the tab is offered, the container mounts, `index_meta` answers
 * 200. Every assertion it makes passes against a stack where the feature is
 * unusable, because it only ever exercises the path where nothing is wrong.
 * This file is the other half.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE SERVER STATES ARE INJECTED, AND WHY THAT IS NOT A WEAKER TEST
 * ─────────────────────────────────────────────────────────────────────────────
 * Both failures are properties of a stack that is MISCONFIGURED — a project
 * with no PgVector configuration, a worker that cannot import a toolkit class.
 * The E2E stack is configured correctly, on purpose, and breaking it for one
 * spec would break every other spec sharing it (only one standalone stack can
 * be up per host — see `scripts/index-stream-e2e.sh`).
 *
 * So the two responses are injected with `page.route`, exactly as
 * `e2e/streaming/index.streaming.spec.ts`'s own negative test aborts the event
 * stream to prove the terminal state is not invented client-side. What is
 * under test here is entirely on the client: whether a real browser, running
 * the real bundle, on the real route, renders a reason. The bodies injected
 * are not invented either — §2's is the byte-for-byte response the parity walk
 * recorded, and §3's is the shape `type_catalogue.go:232-240` writes.
 *
 * §1 is the control, and it is what stops §2 and §3 being vacuous: the SAME
 * toolkit, with nothing intercepted, must still show the working tab. A change
 * that made the tab render an error unconditionally would pass §2 and §3 and
 * fail §1.
 */
import { expect, test } from '@playwright/test';
import type { Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import { BASE_URL } from '../../../playwright.config';

/** The rail's own list request — the one `useIndexesListQuery` issues. */
const INDEX_META_RE = /\/elitea_core\/index_meta\/prompt_lib\//;
/** The served toolkit-type catalogue, which carries the worker-capability verdict. */
const TYPE_CATALOGUE_RE = /\/elitea_core\/toolkits\/prompt_lib\//;

const createdIds: string[] = [];

/**
 * An index-capable toolkit, with its type derived from the LIVE catalogue
 * rather than hardcoded — the same rule J17.5 follows, and for the same
 * reason: a literal here would keep passing after the schema stopped offering
 * `index_data`, which is the defect #296 was about.
 */
async function createIndexCapableToolkit(page: Page): Promise<{ readonly id: string; readonly type: string }> {
  const schemasResp = await page.request.get(`${API_BASE}/elitea_core/toolkits/prompt_lib/${DEFAULT_PROJECT_ID}`);
  expect(schemasResp.status(), await schemasResp.text()).toBe(200);
  const schemas = (await schemasResp.json()) as Record<string, { properties?: { selected_tools?: { args_schemas?: Record<string, unknown> } } }>;

  const indexingType = Object.entries(schemas).find(
    ([, schema]) => schema.properties?.selected_tools?.args_schemas?.['index_data'] !== undefined,
  )?.[0];
  expect(
    indexingType,
    `no toolkit type offers index_data; measured types: ${Object.keys(schemas).join(', ')}`,
  ).toBeTruthy();

  const createResp = await page.request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name: `${AUTOTEST_PREFIX}indexes_explains_${String(Date.now())}`,
      type: indexingType,
      description: 'JRNY-INDEX-A fixture',
      settings: { selected_tools: ['index_data'] },
    },
  });
  expect(createResp.status(), await createResp.text()).toBe(201);
  const created = (await createResp.json()) as { id: string };
  createdIds.push(created.id);
  return { id: created.id, type: indexingType as string };
}

/** Opens the toolkit and clicks through to the Indexes tab, returning the panel. */
async function openIndexesTab(page: Page, toolkitId: string) {
  await page.goto(`${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 20_000 });

  const indexesTab = page.getByRole('tab', { name: 'Indexes' });
  await expect(
    indexesTab,
    'the Indexes tab must still be OFFERED — a vanished tab explains less than a tab with a reason in it',
  ).toBeVisible({ timeout: 20_000 });
  await indexesTab.click();

  const panel = page.getByTestId('edit-toolkit-indexes-tab-panel');
  await expect(panel).toBeVisible({ timeout: 15_000 });
  return panel;
}

test.afterAll(async ({ browser }) => {
  // Best-effort sweep. A leftover `autotest_` toolkit is identifiable by the
  // shared prefix convention, so a failure here must not fail the run.
  if (createdIds.length === 0) return;
  const context = await browser.newContext();
  const page = await context.newPage();
  for (const id of createdIds) {
    await page.request.delete(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`).catch(() => undefined);
  }
  await context.close();
});

/* ── §1. THE CONTROL ─────────────────────────────────────────────────────── */

test('the working tab still works: a healthy read shows the empty state, not an error', async ({ page }) => {
  const toolkit = await createIndexCapableToolkit(page);
  const panel = await openIndexesTab(page, toolkit.id);

  // The two controls a really-mounted `IndexesContainer` produces.
  await expect(panel.getByRole('button', { name: 'Add index' })).toBeVisible({ timeout: 20_000 });
  await expect(panel.getByText('Still no indexes created')).toBeVisible();

  // And the two failure surfaces are ABSENT. Without this pair, §2 and §3
  // would pass against a build that renders an error on every screen.
  await expect(panel.getByTestId('indexes-list-error')).toHaveCount(0);
  await expect(panel.getByTestId('indexes-unavailable')).toHaveCount(0);

  await checkA11y(page);
});

/* ── §2. THE MEASURED FAILED READ ────────────────────────────────────────── */

test('a failed index_meta read names the prerequisite instead of claiming the project is empty', async ({ page }) => {
  const toolkit = await createIndexCapableToolkit(page);

  // The exact response the parity walk recorded, injected on the rail's own
  // request. Everything else on the page — the toolkit, the type catalogue,
  // the form — comes from the real stack.
  await page.route(INDEX_META_RE, (route) =>
    route.fulfill({
      status: 400,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'PGVector configuration is missing for toolkit 1' }),
    }),
  );

  const panel = await openIndexesTab(page, toolkit.id);

  const failure = panel.getByTestId('indexes-list-error');
  await expect(failure, 'a failed read must be reported as a failure').toBeVisible({ timeout: 20_000 });

  // The server's own sentence, verbatim. A generic "could not load" line would
  // keep the failure visible and still throw away the diagnosis, which is the
  // half that makes it actionable.
  await expect(failure).toContainText('PGVector configuration is missing for toolkit 1');
  // The status, so a 502 is not read as a rejection.
  await expect(failure).toContainText('400');
  // And the route out, which the sentence alone does not give.
  await expect(failure).toContainText(/Create a PgVector configuration in Settings/);

  // THE REGRESSION ITSELF. This is the line that fails against the shipped
  // build the parity walk measured.
  await expect(
    panel.getByText('Still no indexes created'),
    'the rail must not assert "no indexes" about a list it could not read',
  ).toHaveCount(0);

  // The user is not trapped: the create affordance stays reachable, so a
  // prerequisite fixed in another tab can be used without a reload.
  await expect(panel.getByRole('button', { name: 'Add index' })).toBeVisible();
});

/* ── §3. THE WORKER-CAPABILITY VERDICT ───────────────────────────────────── */

test('a type the worker cannot run says so, instead of offering a form whose run will die', async ({ page }) => {
  const toolkit = await createIndexCapableToolkit(page);
  const reason = 'the worker does not declare the toolkit class alita_sdk.tools.artifact';

  /*
   * Stamp the verdict onto THIS type in the served catalogue, leaving every
   * other type and every other field untouched — the same three keys
   * `type_catalogue.go:232-240` writes when
   * `workerCapability.SupportsToolkitType` says no. The response is fetched
   * from the real server first and then edited, so the type's real schema
   * (its `args_schemas`, its `index_data` entry) still drives the screen: the
   * ONLY difference from §1 is the metadata block.
   */
  await page.route(TYPE_CATALOGUE_RE, async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as Record<string, Record<string, unknown>>;
    const entry = body[toolkit.type];
    if (entry !== undefined) {
      const metadata = typeof entry['metadata'] === 'object' && entry['metadata'] !== null ? (entry['metadata'] as Record<string, unknown>) : {};
      entry['metadata'] = { ...metadata, hidden: true, unavailable: true, unavailable_reason: reason };
    }
    await route.fulfill({ response, json: body });
  });

  const panel = await openIndexesTab(page, toolkit.id);

  const notice = panel.getByTestId('indexes-unavailable');
  await expect(notice, 'the verdict must reach the screen').toBeVisible({ timeout: 20_000 });
  await expect(notice).toContainText(reason);

  /*
   * And the indexes UI is GONE. This is the point of the change, not a side
   * effect: an "Index" button that dispatches a run onto a worker which has
   * already answered that it cannot import this toolkit class is the silent
   * failure, dressed as a feature. Both controls §1 asserts are checked, so a
   * build that rendered the notice ALONGSIDE a working form fails here.
   */
  await expect(panel.getByRole('button', { name: 'Add index' })).toHaveCount(0);
  await expect(panel.getByText('Still no indexes created')).toHaveCount(0);

  await checkA11y(page);
});
