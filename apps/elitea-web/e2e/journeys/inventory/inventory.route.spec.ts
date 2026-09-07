/**
 * Journey INV-001/002/003: `/inventory` resolves a toolkit, and a toolkit's
 * workspace reads its sources through the facade.
 *
 * ## What this stack can and cannot prove
 *
 * This suite runs against the E2E stack (deploy/docker-compose.e2e-standalone.yml),
 * which DOES run an Inventory provider: `elitea-inventory`, the subapp host
 * with `ELITEA_INVENTORY_RUNNER=fixture`. So unlike the DeepWiki route journey
 * — whose stack has no provider at all and can therefore only read objects
 * somebody seeded — every assertion here goes through the whole path: browser
 * → elitea-main's `/api/v2/inventory/tools/…/invoke` → the facade's source
 * expansion and callback minting → mTLS → the host's admission → the fixture
 * runner's canned graph → poll → parse → screen.
 *
 * It CANNOT prove anything about the real engine. The graph is a constant
 * (services/elitea-subapp-host/internal/apps/inventory/run/fixture.go); what
 * is real is everything around it. It also cannot prove these screens work on
 * the STANDALONE stack, which serves the thin Python fixture runner
 * (services/elitea-inventory/src/elitea_inventory/fixture_runner.py) and
 * answers an EMPTY graph — every data assertion in this directory would fail
 * there, correctly, and that is why there is no standalone project for them.
 *
 * ## Why the assertions are shaped this way
 *
 * A heading is present on a stalled screen too. "Sources", "Graph" and
 * "Statistics" render the instant the workspace mounts, before a single
 * invocation has been answered, and they keep rendering when every one of them
 * 403s. So nothing here asserts a heading: INV-002 names the two citation
 * sources `get_sources_status` reports and the entity counts it gives them,
 * neither of which exists anywhere but in a provider answer.
 */
import { expect, test } from '@playwright/test';

import { STORAGE_STATE } from '../../../playwright.config';
import { openInventory, openReadOnlyWorkspace, SEEDED } from './helpers';

test.describe('Inventory routes', () => {
  test.use({ storageState: STORAGE_STATE.member });
  // Every read is an INVOCATION — start, then poll at 700ms until the provider
  // settles, and the fixture paces each tool at one second per progress step.
  // A journey that opens two tabs therefore spends seconds where a cached GET
  // would spend milliseconds, and the default 30s is not enough for it.
  test.describe.configure({ timeout: 120_000 });

  test('INV-001: /inventory offers the project’s two Inventory toolkits', async ({ page }) => {
    // The project holds TWO, so this route renders the chooser rather than one
    // workspace. Deliberately NOT a redirect — see the route's own header for
    // why the URL must not depend on how many toolkits exist.
    await openInventory(page, '/app/inventory');
    expect(page.url()).toContain('/inventory');

    const chooser = page.getByTestId('inventory-toolkit-chooser');
    await expect(chooser).toBeVisible({ timeout: 30_000 });

    // BY NAME, from the toolkit rows the seed wrote. A chooser that listed the
    // wrong project's toolkits, or that fell back to ids, fails here.
    await expect(chooser.getByText(SEEDED.readOnly.toolkitName, { exact: true })).toBeVisible();
    await expect(chooser.getByText(SEEDED.mutable.toolkitName, { exact: true })).toBeVisible();

    // Neither "nothing to show" state. Both are real screens, and both would
    // mean the listing was not filtered to `type = 'inventory'` as it should be
    // — or was not read at all.
    await expect(page.getByTestId('inventory-no-toolkits')).toHaveCount(0);
    await expect(page.getByTestId('inventory-toolkits-error')).toHaveCount(0);
  });

  test('INV-002: the workspace opens on Sources and reads the provider’s status', async ({
    page,
  }) => {
    await openReadOnlyWorkspace(page);

    // Sources is the tab the workspace opens on, so no click is needed. The
    // PANEL, not the tab label: the label is there before any read lands.
    await expect(page.getByTestId('inventory-sources-panel')).toBeVisible({ timeout: 60_000 });
    const table = page.getByTestId('inventory-sources-table');
    await expect(table).toBeVisible({ timeout: 60_000 });

    // The configured source, by the id the settings name. This row comes from
    // the TOOLKIT's own `sources: [9110]`, so it renders even with every
    // invocation refused — which is exactly why it is not the only assertion.
    await expect(
      page.locator('[data-testid="inventory-source-row"][data-source-id="9110"]'),
    ).toHaveCount(1);

    // …and the two sources the PROVIDER reports having ingested, with the
    // entity counts it gives them. `code` and `docs` are the canned graph's
    // citation labels: they are in no settings row, no seed and no bundle, so
    // only a real `get_sources_status` invocation can put them on this screen.
    // They render as "no longer configured" rows, which is the honest reading —
    // the graph holds their entities and `sources` does not name them.
    for (const [label, entities] of Object.entries(SEEDED.graph.sources)) {
      const row = page.getByTestId('inventory-source-row').filter({ hasText: label });
      await expect(row).toHaveCount(1);
      await expect(row).toContainText(String(entities));
    }

    // And not the empty state, which is what a 403 on the status read leaves
    // behind once the configured rows are stripped.
    await expect(page.getByTestId('inventory-no-sources')).toHaveCount(0);
  });

  test('INV-003: a toolkit that does not exist is reported, not rendered empty', async ({
    page,
  }) => {
    // "This toolkit could not be read" and "this inventory holds no entities"
    // are different facts, and rendering the workspace for an unreadable
    // toolkit would say the second about a graph the screen never learned the
    // bucket of. The DeepWiki port found this as a ROUTING bug — an index route
    // named `deepwiki.tsx` swallowed its `$toolkitId` child and showed the
    // project's own wiki for any id — and `inventory.index.tsx` is named the
    // way it is to start on the other side of that.
    await openInventory(page, '/app/inventory/999999');
    await expect(page.getByTestId('inventory-toolkit-error')).toBeVisible({ timeout: 30_000 });

    // The workspace is not merely hidden behind the banner: it must not have
    // mounted at all, or its panels would be invoking against a toolkit whose
    // settings — and therefore whose bucket — are unknown.
    await expect(page.getByTestId('inventory-workspace')).toHaveCount(0);
    await expect(page.getByTestId('inventory-toolkit-chooser')).toHaveCount(0);
  });
});
