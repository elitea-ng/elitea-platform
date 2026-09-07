/**
 * Journey INV-009: running an ingestion streams progress and reports the
 * objects it wrote.
 *
 * ## Serial safety
 *
 * This is the only spec in the directory that WRITES, and it writes only to
 * toolkit 9102 — whose bucket (`inventory-graph-mutable`) no other journey
 * reads. Everything else in this directory browses 9101. So this file needs no
 * ordering relative to its siblings and takes no lock; it can run in parallel
 * with all of them, which is why there is no `describe.serial` here.
 *
 * ## What this stack can and cannot prove
 *
 * It proves the whole ingest path except the extraction itself:
 *
 *  - THE FACADE EXPANDED THE SOURCE. `run_ingestion` is one of three
 *    `ExpandingTools`: elitea-main resolves `toolkit_id: 9110` against
 *    p_90300.configuration through the project vault, checks the host against
 *    ELITEA_INVENTORY_GIT_ALLOWLIST, mints a callback token and rewrites the
 *    body's `source`. The provider then labels the run `{type}:{id}` from that
 *    rewritten object — so the label `github:9110` in the summary is only
 *    reachable if the expansion happened. An unexpanded body is refused by
 *    CheckSource before any of this.
 *  - THE RUN STREAMED. The six progress lines arrive as `custom_events`, which
 *    are READ-ONCE: a client that awaits the answer and ignores the polls shows
 *    none of them. Their presence on screen is the evidence the poll loop is
 *    draining events rather than merely waiting.
 *  - THE OBJECTS LANDED. The three artifact names come from the TERMINAL body,
 *    which the host writes only after `ComposeResultObjects` and the upload
 *    through the request's own artifact transport. A bucket with no
 *    `elitea_storage.buckets` row 404s that upload.
 *
 * It does NOT prove the graph was extracted from a repository: the fixture
 * runner never clones, and its `access_token` is a literal nothing ever
 * presents to GitHub.
 *
 * ## Why the assertions are shaped this way
 *
 * The banner renders as soon as a run starts, so its presence proves only that
 * a button fired. What proves the run REACHED THE PROVIDER is the summary — a
 * sentence carrying counts and a source label neither this app nor the seed
 * knows — and what proves it reached the BUCKET is the checkpoint object, which
 * is named in the terminal body and nowhere a screen can otherwise reach.
 */
import { expect, test } from '@playwright/test';

import { STORAGE_STATE } from '../../../playwright.config';
import { openInventory, SEEDED } from './helpers';

const GRAPH = SEEDED.graph;
const SOURCE = SEEDED.source;

/** The sentence the fixture runner answers, built from what the request asked for. */
const SUMMARY = `Ingestion completed for ${SOURCE.ingestedAs}: ${GRAPH.entityCount} entities, ${GRAPH.relationCount} relations from 12 files.`;

/** Every object one run lands, by the name the terminal body carries. */
const ARTIFACTS = [
  'graph.json',
  'sources_status.json',
  `.ingestion-checkpoint-${SOURCE.ingestedAs}.json`,
];

test.describe('Inventory ingestion', () => {
  test.use({ storageState: STORAGE_STATE.member });
  // An ingestion is six paced steps plus the poll granularity, on top of the
  // two reads the Sources tab makes before the button can be clicked.
  test.describe.configure({ timeout: 180_000 });

  test('INV-009: an ingestion streams its progress and reports what it wrote', async ({ page }) => {
    await openInventory(page, `/app/inventory/${SEEDED.mutable.toolkitId}`);
    await expect(page.getByTestId('inventory-workspace')).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId('inventory-toolkit-error')).toHaveCount(0);

    // The configured source's row, which is the only one that offers the
    // control: the two orphan rows the provider reports have no toolkit id to
    // send and render no button.
    const sourceRow = page.locator(
      `[data-testid="inventory-source-row"][data-source-id="${SOURCE.toolkitId}"]`,
    );
    await expect(sourceRow).toHaveCount(1, { timeout: 60_000 });

    const runButton = sourceRow.getByTestId('inventory-run-ingestion');
    // ENABLED, which is a fact about the toolkit's settings: the control is
    // disabled without an `llm_model`, and a toolkit seeded without one would
    // make this journey fail on a click that silently did nothing.
    await expect(runButton).toBeEnabled({ timeout: 60_000 });
    await runButton.click();

    const banner = page.getByTestId('inventory-ingestion-banner');
    await expect(banner).toBeVisible({ timeout: 30_000 });

    // The streamed lines. Not "a spinner appeared": these are `custom_events`
    // the provider emitted mid-run, and a client that dropped them would show
    // an indeterminate bar for the whole run and nothing else.
    const steps = page.getByTestId('inventory-ingestion-steps');
    await expect(steps).toBeVisible({ timeout: 60_000 });
    await expect(steps).toContainText('Extracting entities', { timeout: 60_000 });

    // The provider's own summary, carrying the counts it computed and the
    // source label the FACADE's expansion produced.
    await expect(page.getByTestId('inventory-ingestion-summary')).toHaveText(SUMMARY, {
      timeout: 120_000,
    });

    // The three objects, by name. The checkpoint is the one that matters most:
    // `.ingestion-checkpoint-<source>.json` is the record that the run got far
    // enough to be resumable, and its name embeds the same expanded label.
    const artifacts = page.getByTestId('inventory-ingestion-artifact');
    await expect(artifacts).toHaveCount(ARTIFACTS.length, { timeout: 30_000 });
    for (const name of ARTIFACTS) {
      await expect(artifacts.filter({ hasText: name })).toHaveCount(1);
    }

    // The run ENDED, and ended well: the stop control is gone and no refusal
    // banner is on screen. A run that failed also renders a banner with steps
    // in it, which is why "the banner appeared" was never the assertion.
    await expect(page.getByTestId('inventory-stop-ingestion')).toHaveCount(0);
    await expect(banner.getByText(/could not|failed|refused/i)).toHaveCount(0);

    // …and the status read was invalidated and answered again: the source row
    // is still listed rather than the panel having emptied itself on refetch.
    await expect(sourceRow).toHaveCount(1);
  });
});
