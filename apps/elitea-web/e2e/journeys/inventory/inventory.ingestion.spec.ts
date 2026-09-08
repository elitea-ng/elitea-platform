/**
 * Journey INV-009: starting an ingestion, and reading what came back.
 *
 * ## What this journey asserts, and why it does not name one outcome
 *
 * The three tools that name a SOURCE need the facade to expand it, and whether
 * this stack can do that is a property of the stack rather than of the screen:
 * expansion is composed from the Configurations runtime
 * (`internal/api/v2/inventory/inventory.go`'s NewRoute says so in advance), and
 * `deploy/docker-compose.e2e-standalone.yml` has turned that runtime on. Before
 * it did, the provider answered its own refusal — "needs an expanded source" —
 * and this journey pinned that sentence.
 *
 * Pinning it made the journey a statement about ONE deployment. What is being
 * tested is the screen: does a real invocation cross the mTLS hop, does the
 * answer come back READABLE, and does the run settle. All three hold whichever
 * way the expansion goes, so the assertions below are derived from what the
 * run actually did — the pattern `toolkits.credential-status.spec.ts` uses for
 * the same reason.
 *
 * The assertion this file exists for is the readability one, and it found a
 * real defect: the banner rendered
 * `[{"object_type":"message","data":"Run_ingestion failed: …"}]` verbatim.
 * `spi.ToolError` marshals its message into the SAME `result` field a completed
 * run uses, so a refusal is an envelope like any other answer, and
 * `useIngestionRun` was peeling the success and not the failure. The failure
 * was reported and the reason was unreadable — which is the only part of a
 * refusal a user can act on. That assertion is unconditional below.
 *
 * The streamed progress lines and the three objects a completed ingestion
 * lands are still covered where they can be decided deterministically:
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
 * The SPI envelope's own field names.
 *
 * Neither may ever reach the screen: they are the wrapper around the answer,
 * and a banner that shows them has reported a refusal the user cannot read.
 */
const ENVELOPE_KEYS = ['object_type', 'result_target'] as const;

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

    // The run SETTLED, whichever way it went. The stop control is gone and the
    // row offers its button again — a run that never terminated would leave
    // both the other way round, which is a screen that looks busy for ever.
    // This is also the wait: it is the one event both outcomes share.
    await expect(page.getByTestId('inventory-stop-ingestion')).toHaveCount(0, { timeout: 150_000 });
    await expect(runButton).toBeEnabled({ timeout: 30_000 });

    // WHAT CAME BACK, READ FROM THE SCREEN AND NOT ASSUMED. A completed run
    // renders a summary; a refused one renders its reason in the banner. Both
    // are answers this screen must be able to show, and exactly one of them
    // must be here — a run that settled with neither has reported nothing.
    const summary = page.getByTestId('inventory-ingestion-summary');
    if ((await summary.count()) === 1) {
      await expect(summary).toBeVisible();
    } else {
      await expect(
        banner,
        'a run that settled with no summary must say why, in the provider’s own words',
      ).not.toBeEmpty();
    }

    // NOT the envelope, in either case. This is the assertion that would have
    // failed before `useIngestionRun` peeled its refusal: the reason was on
    // screen, wrapped in the SPI result list, and unreadable. `spi.ToolError`
    // uses the same `result` field a completed run does, so this holds for a
    // success as well.
    for (const key of ENVELOPE_KEYS) {
      await expect(banner).not.toContainText(key);
    }

    // The reads were invalidated and answered again: the source row is still
    // listed rather than the panel having emptied itself on the refetch.
    await expect(sourceRow).toHaveCount(1);
  });
});
