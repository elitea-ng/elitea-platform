/**
 * Journey INV-009: starting an ingestion, and reading what came back.
 *
 * ## What this stack can and cannot prove — MEASURED, not assumed
 *
 * The E2E stack does not set `ELITEA_CONFIGURATIONS_ENABLED`, so elitea-main
 * composes no Configurations runtime, so it has no toolkit-settings resolver,
 * so the Inventory facade mounts WITHOUT source expansion. It says so at boot,
 * in as many words:
 *
 *   Inventory mounts without source expansion  settings_resolver=false
 *
 * That is the facade's designed behaviour for a stack with no vault
 * (internal/api/v2/inventory/inventory.go's NewRoute comment says it in
 * advance): the eight tools that only READ the graph work, and the three that
 * name a source get the PROVIDER's own refusal rather than a facade that
 * quietly forwards an unexpanded toolkit id.
 *
 * So this journey asserts the path that exists here, end to end:
 *
 *   the control is offered and enabled → the click starts a real invocation
 *   → the facade forwards it → the provider refuses it, by name and with a
 *   reason → the screen shows THAT REASON, readably → the run settles and the
 *   controls come back.
 *
 * The last two are the ones worth having, and the third found a defect the
 * first version of this file was written to catch by accident: the banner
 * rendered `[{"object_type":"message","data":"Run_ingestion failed: …"}]`
 * verbatim. `spi.ToolError` marshals its message into the SAME `result` field
 * a completed run uses, so a refusal is an envelope like any other answer, and
 * `useIngestionRun` was peeling the success and not the failure. The failure
 * was reported and the reason was unreadable — which is the only part of a
 * refusal a user can act on.
 *
 * WHAT IS NOT PROVED HERE, and where it would be. A COMPLETED ingestion — the
 * six streamed progress lines, the summary carrying the expanded `{type}:{id}`
 * label, and the three objects it lands (graph.json, sources_status.json and
 * `.ingestion-checkpoint-<source>.json`) — needs a stack whose facade can
 * expand a source: one with `ELITEA_CONFIGURATIONS_ENABLED=true` and a project
 * vault, which is the standalone stack. Turning the Configurations runtime on
 * in `deploy/docker-compose.e2e-standalone.yml` is what would move that half
 * here, and it is a change to every journey on this stack rather than to this
 * one. Until then the streaming half is covered where it can be decided:
 * `src/features/inventory-sources/model/useIngestionRun.test.tsx` replays the
 * read-once `custom_events` and the terminal artifact list.
 *
 * ## Serial safety
 *
 * This is the only spec in the directory that CAN write, and it addresses only
 * toolkit 9102 — whose bucket (`inventory-graph-mutable`) no other journey
 * reads. It needs no ordering relative to its siblings and takes no lock.
 */
import { expect, test } from '@playwright/test';

import { STORAGE_STATE } from '../../../playwright.config';
import { openInventory, SEEDED } from './helpers';

const SOURCE = SEEDED.source;

/**
 * The provider's refusal, in its own words.
 *
 * It comes from the HOST's source check
 * (services/elitea-subapp-host/internal/apps/inventory/run — CheckSource), not
 * from this application and not from the facade, so a screen that produced it
 * has carried a real invocation all the way through the mTLS hop and back.
 */
const REFUSAL = /needs an expanded source/i;

test.describe('Inventory ingestion', () => {
  test.use({ storageState: STORAGE_STATE.member });
  // Two reads settle before the button can be clicked, and the run itself is
  // an invoke plus a poll; the fixture paces each step at a second.
  test.describe.configure({ timeout: 180_000 });

  test('INV-009: starting an ingestion reports the provider’s own answer, readably', async ({
    page,
  }) => {
    await openInventory(page, `/app/inventory/${SEEDED.mutable.toolkitId}`);
    await expect(page.getByTestId('inventory-workspace')).toBeVisible({ timeout: 30_000 });
    await expect(page.getByTestId('inventory-toolkit-error')).toHaveCount(0);

    // The configured source's row, which is the only one that offers the
    // control: a row the toolkit no longer names has no id to send and renders
    // no button.
    const sourceRow = page.locator(
      `[data-testid="inventory-source-row"][data-source-id="${SOURCE.toolkitId}"]`,
    );
    await expect(sourceRow).toHaveCount(1, { timeout: 60_000 });

    const runButton = sourceRow.getByTestId('inventory-run-ingestion');
    // ENABLED, which is a fact about the toolkit's settings rather than about
    // the button: the control is disabled without an `llm_model`, and a
    // toolkit seeded without one would make this journey pass on a click that
    // silently did nothing.
    await expect(runButton).toBeEnabled({ timeout: 60_000 });
    await runButton.click();

    // A run started. The banner renders as soon as one does, so on its own it
    // proves only that the button fired — which is why it is not the assertion
    // this journey rests on.
    const banner = page.getByTestId('inventory-ingestion-banner');
    await expect(banner).toBeVisible({ timeout: 30_000 });

    // THE PROVIDER ANSWERED, AND THE SCREEN CAN BE READ. The sentence is the
    // host's own; nothing in this application, this seed or this bundle
    // contains it, so it is only reachable through invoke → mTLS → admission →
    // the source check → poll → peel → render.
    await expect(banner).toContainText(REFUSAL, { timeout: 120_000 });

    // NOT the envelope. This is the assertion that would have failed before
    // `useIngestionRun` peeled its refusal: the reason was on screen, wrapped
    // in the SPI result list, and unreadable.
    await expect(banner).not.toContainText('object_type');
    await expect(banner).not.toContainText('result_target');

    // The run SETTLED. The stop control is gone and the row offers its button
    // again — a run that never terminated would leave both the other way
    // round, which is a screen that looks busy for ever.
    await expect(page.getByTestId('inventory-stop-ingestion')).toHaveCount(0, { timeout: 30_000 });
    await expect(runButton).toBeEnabled({ timeout: 30_000 });

    // …and no summary was invented for a run that did not complete. A screen
    // that reported both a refusal and a result is describing two runs.
    await expect(page.getByTestId('inventory-ingestion-summary')).toHaveCount(0);

    // The reads were invalidated and answered again: the source row is still
    // listed rather than the panel having emptied itself on the refetch.
    await expect(sourceRow).toHaveCount(1);
  });
});
