/**
 * Journey INV-007/008: the Statistics tab reports the graph's own numbers, and
 * a maintenance tool runs and reports what the provider said.
 *
 * ## What this stack can and cannot prove
 *
 * `get_stats`, `get_cache_stats` and `normalize_types` are all invocations
 * against the E2E stack's `elitea-inventory` service, so what is proved is the
 * whole path — including that `normalize_types` is sent WITHOUT
 * `output_format: 'json'` and its plain sentence is what reaches the screen.
 * What is not proved is the engine's own arithmetic: the counts below are the
 * fixture runner's constants.
 *
 * INV-008 also cannot prove that normalising CHANGED anything, because on this
 * graph it does not: the fixture answers "Types are already normalised", which
 * is a real terminal answer and the honest thing to assert. A journey that
 * demanded a mutation would need a graph with `Feature` and `feature` in it,
 * which the fixture deliberately does not have.
 *
 * ## Why the assertions are shaped this way
 *
 * The counters render `—` for a number nobody reported, and that is what a
 * mounted-but-unread panel shows. So INV-007 asserts the EXACT numerals, and
 * the type breakdown by its chip labels: `class: 3` is the product of the
 * provider's `entities_by_type` and this app's own descending-count ordering,
 * and neither half alone can produce it.
 *
 * INV-008 asserts the provider's SENTENCE, not that a button was clickable. A
 * button that fires nothing leaves the result line absent, and a button whose
 * answer is shown unpeeled prints the SPI envelope — a wall of escaped JSON —
 * which is what the first version of the ingestion hook did.
 */
import { expect, test } from '@playwright/test';

import { STORAGE_STATE } from '../../../playwright.config';
import { openReadOnlyWorkspace, openTab, SEEDED } from './helpers';

const GRAPH = SEEDED.graph;

/** What `normalize_types` answers for a graph that needs no normalising. */
const NORMALISED_SENTENCE = `Types are already normalised: ${Object.keys(GRAPH.byType).length} distinct types.`;

test.describe('Inventory statistics', () => {
  test.use({ storageState: STORAGE_STATE.member });
  test.describe.configure({ timeout: 120_000 });

  test('INV-007: the Statistics tab shows the counts and the type breakdown', async ({ page }) => {
    await openReadOnlyWorkspace(page);
    await openTab(page, 'stats');

    const panel = page.getByTestId('inventory-stats-panel');
    await expect(panel).toBeVisible({ timeout: 60_000 });
    await expect(page.getByTestId('inventory-stats-error')).toHaveCount(0);

    // The two counts, EXACTLY. `—` is what an unread panel renders, and `0`
    // is what a panel that mistook "not reported" for "none" would render;
    // both are ruled out by naming the numerals.
    await expect(page.getByTestId('inventory-stat-entities')).toHaveText(
      String(GRAPH.entityCount),
    );
    await expect(page.getByTestId('inventory-stat-relations')).toHaveText(
      String(GRAPH.relationCount),
    );

    // The breakdown, by chip. Each label is `{type}: {count}` and the three
    // together are the provider's `entities_by_type` object read through this
    // app's ordering — a breakdown that lost the counts, or that rendered the
    // raw object, fails on the first one.
    const byType = page.getByTestId('inventory-stats-by-type');
    await expect(byType).toBeVisible();
    for (const [name, count] of Object.entries(GRAPH.byType)) {
      await expect(byType.getByText(`${name}: ${count}`, { exact: true })).toHaveCount(1);
    }

    // The sources the graph's citations carry. Same two labels INV-002 finds on
    // the Sources tab, reached here through a DIFFERENT tool — so a stack where
    // only one of the two reads works cannot pass both journeys.
    const sources = page.getByTestId('inventory-stats-sources');
    for (const label of Object.keys(GRAPH.sources)) {
      await expect(sources.getByText(label, { exact: true })).toHaveCount(1);
    }
  });

  test('INV-008: “Normalise types” runs and reports what the provider said', async ({ page }) => {
    await openReadOnlyWorkspace(page);
    await openTab(page, 'stats');
    await expect(page.getByTestId('inventory-stats-panel')).toBeVisible({ timeout: 60_000 });

    // Nothing has been run yet, so there is no result line to mistake for one.
    await expect(page.getByTestId('inventory-maintenance-result')).toHaveCount(0);

    await page.getByTestId('inventory-normalize-types').click();

    // The PROVIDER'S OWN SENTENCE, verbatim. It names a number the fixture
    // computed from the graph (three distinct types), so a screen that
    // fabricated a success message, or that showed the envelope instead of the
    // result, cannot produce it.
    await expect(page.getByTestId('inventory-maintenance-result')).toHaveText(
      NORMALISED_SENTENCE,
      { timeout: 60_000 },
    );

    // The run settled: the button is offered again. It is disabled while a
    // maintenance tool is in flight, so a run that never terminated would leave
    // it disabled forever — a screen that looks finished and is not.
    await expect(page.getByTestId('inventory-normalize-types')).toBeEnabled({ timeout: 30_000 });

    // …and the counts survived the invalidation the run triggers. Every read of
    // this toolkit is invalidated when a maintenance tool finishes, so this is
    // also the assertion that the refetch answered rather than emptying the
    // panel.
    await expect(page.getByTestId('inventory-stat-entities')).toHaveText(
      String(GRAPH.entityCount),
      { timeout: 60_000 },
    );
  });
});
