import { expect, type Locator, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

/**
 * What scripts/e2e-stack.sh seeds for these journeys (project 90300).
 *
 * TWO TOOLKITS, AND THE COUNT IS PART OF THE FIXTURE. `/inventory` renders one
 * toolkit outright and offers a CHOOSER for several, so INV-001 asserts the
 * chooser and would fail if a third toolkit — or only one — were seeded. That
 * is also why Inventory has its own project rather than sharing DeepWiki's
 * 90200: the count would otherwise be a function of what another application's
 * fixtures happen to add.
 *
 * Toolkit 9101 is READ-ONLY across the suite: INV-002 and INV-004–008, 010
 * browse it and never write. Toolkit 9102 is the one INV-009 ingests into, and
 * its bucket differs so the objects it lands cannot touch anything 9101 reads.
 * Spec files run in any order and in parallel.
 *
 * THE GRAPH IS NOT SEEDED. Every value under `graph` below comes from the
 * fixture RUNNER — services/elitea-subapp-host/internal/apps/inventory/run/
 * fixture.go, which `ELITEA_INVENTORY_RUNNER=fixture` selects — and reaches
 * the browser only through a real invocation of the facade. Changing the
 * runner's canned graph without changing these constants is a red suite, which
 * is the point: they are what makes "the screen read the provider" an
 * assertion with a failure mode rather than a screenshot of a mounted panel.
 */
export const SEEDED = {
  projectId: '90300',
  projectName: 'e2e-inventory',
  readOnly: {
    toolkitId: '9101',
    toolkitName: 'E2E Inventory',
    bucket: 'inventory-graph',
  },
  mutable: {
    toolkitId: '9102',
    toolkitName: 'E2E Inventory Mutable',
    bucket: 'inventory-graph-mutable',
  },
  /** The one source toolkit both Inventory toolkits name in `sources`. */
  source: {
    toolkitId: '9110',
    type: 'github',
    /**
     * The label an ingestion reports for it: `{type}:{id}`, built by
     * `SourceLabelFor` from the source object THE FACADE EXPANDED
     * (material/source.go writes `{"toolkit_id": …, "type": …}`). Asserting on
     * it is how INV-009 proves the expansion happened — a provider that
     * received an unexpanded `toolkit_id` would report a different label, and
     * one that received no source at all is refused by CheckSource.
     */
    ingestedAs: 'github:9110',
  },
  /** The canned graph, as the fixture runner answers it. */
  graph: {
    entityCount: 6,
    relationCount: 5,
    /** Every entity name, in no particular order. */
    names: [
      'CheckoutService',
      'place_order',
      'Order',
      'PaymentClient',
      'Checkout guide',
      'Payments guide',
    ],
    /** `entities_by_type`, which is also what the type facet offers. */
    byType: { class: 3, document: 2, function: 1 },
    /** The two citation sources `get_sources_status` reports, with their entity counts. */
    sources: { code: 4, docs: 2 },
    /** The entity INV-006 opens, and the two edges that reach it. */
    hub: {
      id: 'code:checkout-service',
      name: 'CheckoutService',
      /** `code:place-order` outgoing (defines), `docs:checkout-guide` incoming (documents). */
      neighbours: ['code:place-order', 'docs:checkout-guide'],
    },
    /**
     * The neighbour INV-006 walks to, so the click is checked by what it
     * opened. `docs:checkout-guide` and not `code:place-order`: the latter
     * shares the hub's `file_path` (both are in src/checkout/service.py), so a
     * pane that never refetched would show the right path for the wrong
     * reason. This one's differs in every field.
     */
    neighbour: { id: 'docs:checkout-guide', name: 'Checkout guide', filePath: 'docs/checkout.md' },
  },
} as const;

export const ERROR_BOUNDARY_TEXT = /something went wrong|unexpected error/i;
export const NOT_FOUND_TEXT = /not found|404/i;

/** Opens an Inventory route with project 90300 selected, and asserts the shell did not fall over. */
export async function openInventory(page: Page, path: string): Promise<void> {
  await page.addInitScript(
    ([id, name]) => {
      localStorage.setItem('el.project.id', id);
      localStorage.setItem('el.project.name', name);
      sessionStorage.setItem('el.project.id', id);
      sessionStorage.setItem('el.project.name', name);
    },
    [SEEDED.projectId, SEEDED.projectName] as const,
  );
  await page.goto(`${BASE_URL}${path}`, { waitUntil: 'domcontentloaded' });
  // BACK ON THE APP ORIGIN, not merely idle: an auth redirect is a navigation,
  // and a URL read mid-hop reports the identity provider rather than a routing
  // decision. `networkidle` alone let this pass on webkit and fail on chromium
  // in the DeepWiki suite, which is where the rule comes from.
  await page.waitForURL((url) => url.origin === new URL(BASE_URL).origin, { timeout: 30_000 });
  await page.waitForLoadState('networkidle');
  await expect(page.getByText(NOT_FOUND_TEXT)).toHaveCount(0);
  await expect(page.getByText(ERROR_BOUNDARY_TEXT)).toHaveCount(0);
}

/** Opens toolkit 9101's workspace and waits for the tab bar to be live. */
export async function openReadOnlyWorkspace(page: Page): Promise<void> {
  await openInventory(page, `/app/inventory/${SEEDED.readOnly.toolkitId}`);
  await expect(page.getByTestId('inventory-workspace')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('inventory-toolkit-error')).toHaveCount(0);
}

/**
 * Switches to one workspace tab.
 *
 * It does NOT wait for the panel, and the caller must: a tab that highlights
 * without mounting its panel is a screen that looks switched and reads
 * nothing, so the evidence belongs in the test that names what it expected.
 */
export async function openTab(page: Page, tab: 'sources' | 'graph' | 'stats'): Promise<void> {
  await page.getByTestId(`inventory-tab-${tab}`).click();
}

/**
 * Sets one of the three graph facets.
 *
 * The facets are MUI `select` TextFields: what carries the test id is the
 * control's own input, and what OPENS the menu is the `combobox` beside it in
 * the same `MuiInputBase-root`. Clicking the test id alone selects nothing and
 * the list stays as it was — a passing "the filter did nothing" assertion.
 */
export async function chooseFacet(
  page: Page,
  facet: 'type' | 'layer' | 'source',
  option: string,
): Promise<void> {
  const marked = page.getByTestId(`inventory-filter-${facet}`);
  await expect(marked).toHaveCount(1);
  const control = marked.locator(
    'xpath=ancestor-or-self::*[contains(concat(" ", @class, " "), " MuiInputBase-root ")][1]',
  );
  await control.getByRole('combobox').click();
  await page.getByRole('option', { name: option, exact: true }).click();
}

/**
 * One entity row, addressed by the id the list stamps on it.
 *
 * BY ID AND NOT BY NAME: `Order` is a substring of no other name here today,
 * but `Checkout guide` and `CheckoutService` both contain "Checkout", and a
 * text locator that matched two rows would fail for a reason that has nothing
 * to do with the graph.
 */
export function entityRow(page: Page, entityId: string): Locator {
  return page.locator(`[data-testid="inventory-entity-row"][data-entity-id="${entityId}"]`);
}

