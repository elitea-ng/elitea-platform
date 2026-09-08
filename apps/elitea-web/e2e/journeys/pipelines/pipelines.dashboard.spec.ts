/**
 * Journey 16d: the pipelines DASHBOARD — the list, its search box, and what a
 * deletion does to both.
 *
 * Ported BY USE CASE from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/pipelines/
 * test_pipeline_management.py`), not one-for-one; each test below names the
 * legacy test it answers. Every existing pipelines journey starts on
 * `/app/pipelines/create` and never looks at the list it came from, so the
 * whole dashboard — the tab bar, the card grid, the search field, and the row
 * disappearing after a delete — had no coverage at all.
 *
 * ── Why the pipelines get created through the API here ───────────────────
 *
 * These are claims about a LIST, not about authoring. `createPipelineThroughApi`
 * (`e2e/fixtures/pipelines.ts`) writes the same `agent_type: 'pipeline'`
 * version the create form does, which is the field every route on this page
 * filters by, and it costs one request instead of a form round trip plus a
 * canvas mount per test.
 *
 * ── The one property of this list every assertion below depends on ───────
 *
 * `PrivatePipelinesList.tsx` reads ONE page of the applications list (the
 * handler defaults `limit` to 20 and the generated params cannot ask for
 * more — that file's own doc comment records the spec gap), and its search box
 * then filters THAT page client-side. The rows come back
 * `ORDER BY a.id DESC` (`internal/infra/db/repos/applications.go:271`), so a
 * pipeline created moments earlier is the FIRST row and is on the page
 * whatever else the project holds. A journey that created its row and then
 * did something else for a while would not be able to rely on that, which is
 * why each test below creates immediately before it looks.
 *
 * ── Two legacy use cases have no port target, and that is a product gap ──
 *
 *  - `TestPipelineDashboard::test_view_toggle_table_and_card` — there is no
 *    table view to switch to. `pages/pipelines/Pipelines.tsx`'s own module
 *    comment discloses it: `ViewToggle` (`@/components/ViewToggle` in the
 *    baseline) has no port anywhere in `shared/ui` or `widgets`. The agents
 *    port (`agents.editor.spec.ts`) records the same gap for the same
 *    control.
 *  - `TestDeletePipeline::test_delete_pipeline_via_ui_menu` — a pipeline
 *    cannot be deleted from the UI at all. `pages/pipelines/ui/
 *    EditPipelineActions.tsx` states it ("export/delete have no pipeline-side
 *    mount point yet"): the editor's `⋮` menu is `EntityLifecycleMenu`, which
 *    carries Share/Fork only, the version bar's Delete removes a VERSION, and
 *    the list cards carry no menu. The delete USE CASE is covered below
 *    through the API, which is the half that exists.
 *
 * Neither is written as an `if (await control.isVisible())` journey: a test
 * that skips itself when the control is missing reports green on the app that
 * has nothing, which is the shape this suite exists to refuse.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';
import { DEFAULT_PROJECT_NAME, ensureProjectSelected } from '../../fixtures/project';
import { createPipelineThroughApi, deletePipeline, type CreatedPipeline } from '../../fixtures/pipelines';

/** Pipelines minted by the tests below, removed in `afterEach` whatever happened. */
const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline === undefined) continue;
    await deletePipeline(page.request, pipeline);
  }
});

/** A name unique per run, inside the 32-character ceiling the create form imposes on the same column. */
function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}-${String(Date.now()).slice(-7)}`;
}

/**
 * Open the private pipelines list and wait for it to be a LIST, not a spinner.
 *
 * `ensureProjectSelected` first: the tab set is chosen from the selected
 * project (`usePipelineTabs`), and the public project renders
 * Latest/My liked/Trending instead of the single "All" tab this route needs.
 * The personas are pinned to the seeded project by `auth.setup.ts`, so this is
 * a guard against that pin drifting rather than a switch per call.
 */
async function openPipelinesList(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/app/pipelines/all`);
  await ensureProjectSelected(page, DEFAULT_PROJECT_NAME);
  await page.waitForURL('**/app/pipelines/all**', { timeout: 20_000 });
  await expect(searchBox(page), 'the pipelines list must render its search box').toBeVisible({ timeout: 30_000 });
}

/**
 * The list's own search field — `PrivatePipelinesList.tsx`'s `SimpleSearchBar`,
 * placeholder "Search".
 *
 * NOTE FOR THE SERVER-SIDE SEARCH UNIT (product-gaps wave): this control
 * exists today and filters the ONE page the list reads, client-side. The
 * assertions below are written against the observable behaviour — "the
 * matching row survives, the others go" — and not against where the filtering
 * happens, so a server-side query behind the same box keeps them true. What
 * DOES need revisiting when that lands is this file's header note about the
 * single 20-row page, and the decoy row J16d creates to make its own
 * precondition: with a server-side query the decoy stays correct but its
 * "same page" justification no longer applies.
 */
function searchBox(page: Page) {
  return page.getByPlaceholder('Search', { exact: true });
}

/** Every card currently drawn by `EntityCardList`. */
function cards(page: Page) {
  return page.getByTestId('entity-card');
}

/*
 * Legacy: `TestPipelineDashboard::test_pipeline_dashboard_loads` ("header and
 * search input are visible"), `TestPipelineDashboard::
 * test_pipeline_created_via_api_visible_in_dashboard`, and
 * `TestSearchPipeline::test_search_pipeline_by_name`.
 *
 * One journey rather than three because the middle one is the precondition of
 * the last and the first is the precondition of both — and because a "the
 * header is visible" test on its own asserts nothing a blank page with a tab
 * bar would fail.
 *
 * The header this app renders is the TAB BAR, not a title: `PageHeader`
 * ignores its `title` prop whenever `tabs` is set (`widgets/page-header`), so
 * the accessible name of the tablist is what identifies this screen.
 */
test('J16d: the dashboard lists a pipeline the API created, and the search box narrows the list to it', async ({
  page,
}) => {
  // THE SECOND ROW IS MINTED HERE, NOT ASSUMED FROM THE SEED — read this
  // before deleting it as duplication of the row below.
  //
  // The "narrowed to one" claim at the end of this test is only a claim about
  // the SEARCH if the list held more than one row before the query. The first
  // CI read of this journey asserted that precondition against whatever the
  // project happened to hold and failed on webkit with `Received: 1`: the
  // e2e seed creates no pipelines, so the only other rows on that list are the
  // ones sibling journeys have created and not yet torn down. Which of them
  // overlap this test is decided by worker scheduling, so the precondition was
  // passing on borrowed state. The decoy makes it this test's own.
  //
  // Created BEFORE the row under test so that, under the list's
  // `ORDER BY a.id DESC`, both sit at the top of the single page
  // `PrivatePipelinesList` reads. Its name shares no substring with the
  // searched one, so it is a row the search must drop.
  const decoy = await createPipelineThroughApi(page.request, uniqueName('decoy'));
  created.push(decoy);

  const name = uniqueName('dash');
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);

  await openPipelinesList(page);
  await expect(page.getByRole('tablist', { name: 'Pipelines' })).toBeVisible();

  const row = cards(page).filter({ hasText: name });
  await expect(row, 'a pipeline created through the API must appear on the dashboard').toHaveCount(1, {
    timeout: 30_000,
  });

  // At least one OTHER row is on screen before the search — otherwise
  // "narrowed to one" below is satisfied by a project that only ever had one
  // pipeline, and this test would prove nothing about the search box. The
  // decoy created at the top of this test guarantees it. Polled rather than
  // read once: `count()` does not wait for attach, and the two cards can be
  // painted a frame apart.
  await expect
    .poll(() => cards(page).count(), { timeout: 20_000 })
    .toBeGreaterThan(1);

  await searchBox(page).fill(name);

  // `toHaveCount` retries, which is what covers `SimpleSearchBar`'s 300 ms
  // debounce without a `waitForTimeout`. Exactly ONE: `matchesQuery`
  // (`PrivatePipelinesList.tsx:41`) matches on `name` alone and this name
  // carries a per-run suffix, so any second card here is a search that did
  // not filter.
  await expect(cards(page), 'the search must narrow the list to the one matching pipeline').toHaveCount(1, {
    timeout: 20_000,
  });
  await expect(cards(page).first()).toContainText(name);
});

/*
 * Legacy: `TestSearchPipeline::test_search_pipeline_no_results`.
 *
 * The legacy assertion is an absence ("the pipeline is not in the results"),
 * which a search box that silently ignored its input would also satisfy on a
 * project that never held that name. This asserts the state the app actually
 * enters instead: zero cards AND the empty state the list swaps in for a
 * query that matched nothing, whose copy differs from the "no pipelines yet"
 * one (`PrivatePipelinesList.tsx`'s `emptyTitle` ternary).
 */
test('J16d: a search that matches nothing empties the list and says so', async ({ page }) => {
  await openPipelinesList(page);

  await searchBox(page).fill('zzzz_nonexistent_pipeline_12345');

  // `.count()` for the absence — it does not wait-for-attach the way a
  // visibility assertion does.
  await expect(cards(page)).toHaveCount(0, { timeout: 20_000 });
  await expect(page.getByText('Nothing found.')).toBeVisible({ timeout: 10_000 });

  // …and the empty state is the SEARCH one, not the "this project has no
  // pipelines" one. Without this the test passes against a list that dropped
  // its rows for any other reason.
  await expect(page.getByText('No pipelines yet')).toHaveCount(0);
});

/*
 * Legacy: `TestDeletePipeline::test_delete_pipeline_via_api` — "create a
 * pipeline, delete it through the API, and verify it is gone from the UI".
 *
 * The DELETE is checked on the server first and on the screen second. A list
 * that still showed the row would otherwise be indistinguishable from a
 * delete the backend refused, and the two need different fixes.
 */
test('J16d: a pipeline deleted through the API leaves the dashboard', async ({ page }) => {
  const name = uniqueName('del');
  const pipeline = await createPipelineThroughApi(page.request, name);

  await openPipelinesList(page);
  await expect(cards(page).filter({ hasText: name })).toHaveCount(1, { timeout: 30_000 });

  await deletePipeline(page.request, pipeline);

  // The server's own read is the proof the row is gone; polled because the
  // delete is not synchronous with the next read.
  await expect
    .poll(
      async () => {
        const detail = await page.request.get(
          `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${pipeline.projectId}/${pipeline.id}`,
        );
        return detail.status();
      },
      { timeout: 20_000, message: 'the pipeline was still readable after its DELETE' },
    )
    .toBeGreaterThanOrEqual(400);

  // Then the screen. A reload, not a cache invalidation: this journey deleted
  // through the API, so the page was never told.
  await openPipelinesList(page);
  await expect(cards(page).filter({ hasText: name })).toHaveCount(0, { timeout: 30_000 });
});
