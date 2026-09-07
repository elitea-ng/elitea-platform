/**
 * Journey INV-004/005/006: the graph tab lists what the provider holds,
 * narrows it, and walks from one entity to the next.
 *
 * ## What this stack can and cannot prove
 *
 * The E2E stack runs `elitea-inventory` with `ELITEA_INVENTORY_RUNNER=fixture`,
 * so every list here is the answer to a real invocation — `search_graph`,
 * `list_entities_by_type`, `get_entity`, `get_entity_neighbors` — carried
 * through the facade, the mTLS hop, the host's admission and the poll loop.
 * What it cannot prove is that the ENGINE answers the same shapes; the canned
 * graph is a constant and only the path around it is production code.
 *
 * It also proves nothing on the standalone stack, whose Python fixture runner
 * answers an empty graph. These specs are chromium/webkit only for that reason.
 *
 * ## Why the assertions are shaped this way
 *
 * `inventory-entity-list` renders for one row and for two hundred, so its mere
 * presence says nothing. Every assertion below names DATA:
 *
 *  - INV-004 names `CheckoutService` and counts six rows. The name exists in no
 *    bundle, no seed and no settings row — only in the provider's graph — and
 *    the count is what separates "the list read the graph" from "the list read
 *    one lucky row".
 *  - INV-005 narrows to `class` and requires the list to LOSE `place_order`.
 *    A filter that did nothing leaves all six rows and passes any assertion
 *    that only checks the survivors are still there; the disappearance is the
 *    half with a failure mode. It also proves the filtering happened on the
 *    PROVIDER — `list_entities_by_type` is a different tool from `search_graph`
 *    — because a client-side filter over a fetched page would have narrowed the
 *    same rows and cannot be told apart any other way than by the count of
 *    three, which is the whole graph's `class` count and not a page's.
 *  - INV-006 opens an entity and then walks a NEIGHBOUR, and checks the walk by
 *    what the pane shows afterwards. A neighbour rendered as a label rather
 *    than a link is the defect this catches: the list still shows the edge, and
 *    the click does nothing at all.
 */
import { expect, test } from '@playwright/test';

import { STORAGE_STATE } from '../../../playwright.config';
import { chooseFacet, entityRow, openReadOnlyWorkspace, openTab, SEEDED } from './helpers';

const GRAPH = SEEDED.graph;

test.describe('Inventory graph', () => {
  test.use({ storageState: STORAGE_STATE.member });
  // Reads are invocations paced at a second per progress step; a test that
  // opens a tab and then clicks twice spends most of its time waiting on the
  // provider. See the route spec's note.
  test.describe.configure({ timeout: 120_000 });

  test('INV-004: the Graph tab lists the entities the provider holds', async ({ page }) => {
    await openReadOnlyWorkspace(page);
    await openTab(page, 'graph');

    const list = page.getByTestId('inventory-entity-list');
    await expect(list).toBeVisible({ timeout: 60_000 });

    // The whole graph, because the filter is empty and the provider reads an
    // empty query as "everything". Six is the canned graph's node count.
    await expect(page.getByTestId('inventory-entity-row')).toHaveCount(GRAPH.entityCount, {
      timeout: 60_000,
    });

    // Every entity BY NAME. `CheckoutService` in particular is data only a
    // real read can produce — it is in no bundle, no route and no seed row.
    for (const name of GRAPH.names) {
      await expect(list.getByText(name, { exact: true })).toHaveCount(1);
    }

    // Not the empty state, and not a refusal. Both render a screen with the
    // tab bar intact, which is what makes them worth naming.
    await expect(page.getByTestId('inventory-entities-empty')).toHaveCount(0);
    await expect(page.getByTestId('inventory-entities-error')).toHaveCount(0);
  });

  test('INV-005: the type facet narrows the list to what the provider filtered', async ({
    page,
  }) => {
    await openReadOnlyWorkspace(page);
    await openTab(page, 'graph');
    await expect(page.getByTestId('inventory-graph-filters')).toBeVisible({ timeout: 60_000 });
    await expect(page.getByTestId('inventory-entity-row')).toHaveCount(GRAPH.entityCount, {
      timeout: 60_000,
    });

    // `place_order` is the graph's only `function`, so it is the row whose
    // ABSENCE afterwards says the narrowing happened.
    await expect(entityRow(page, 'code:place-order')).toHaveCount(1);

    // The facet OFFERS ONLY WHAT THE GRAPH HOLDS: the options come from
    // `get_stats`, not from a hard-coded legacy type list. Choosing `class` by
    // its exact label therefore also asserts the facet was populated from a
    // provider answer.
    await chooseFacet(page, 'type', 'class');

    await expect(page.getByTestId('inventory-entity-row')).toHaveCount(GRAPH.byType.class, {
      timeout: 60_000,
    });
    await expect(entityRow(page, 'code:place-order')).toHaveCount(0);
    // The three that remain, named. A narrowing that dropped the wrong rows
    // would still count three on some other graph; on this one it cannot.
    for (const id of ['code:checkout-service', 'code:order-model', 'code:payment-client']) {
      await expect(entityRow(page, id)).toHaveCount(1);
    }
  });

  test('INV-006: an entity opens with its relations, and a neighbour opens in turn', async ({
    page,
  }) => {
    await openReadOnlyWorkspace(page);
    await openTab(page, 'graph');
    await expect(entityRow(page, GRAPH.hub.id)).toBeVisible({ timeout: 60_000 });

    // Before the click the pane says nothing has been chosen — which is a
    // different screen from "this entity has no relations", and mixing the two
    // is what the pane's own header warns about.
    await expect(page.getByTestId('inventory-entity-none')).toBeVisible();

    await entityRow(page, GRAPH.hub.id).click();

    const detail = page.getByTestId('inventory-entity-detail');
    await expect(detail).toBeVisible({ timeout: 60_000 });
    await expect(detail).toContainText(GRAPH.hub.name);
    await expect(page.getByTestId('inventory-entity-id')).toHaveText(GRAPH.hub.id);

    // The RELATIONS, which is the second read — `get_entity_neighbors`, a
    // different tool with a different argument name from `get_entity`. A pane
    // that showed the entity and no edges would pass every assertion above.
    const relations = page.getByTestId('inventory-entity-relations');
    await expect(relations).toBeVisible({ timeout: 60_000 });
    await expect(page.getByTestId('inventory-neighbour')).toHaveCount(
      GRAPH.hub.neighbours.length,
    );
    for (const neighbourId of GRAPH.hub.neighbours) {
      await expect(relations.getByText(neighbourId, { exact: true })).toHaveCount(1);
    }
    // The DIRECTION, which is what makes an edge readable: `defines` points
    // away from the hub and `documents` points at it, and a pane that dropped
    // the arrow renders a caller and a callee identically.
    await expect(relations).toContainText('→ defines');
    await expect(relations).toContainText('← documents');
    await expect(page.getByTestId('inventory-entity-no-relations')).toHaveCount(0);

    // Walking the edge. The neighbour is a LINK, and this is the assertion
    // that fails when it is rendered as a label: nothing happens, and the pane
    // keeps describing the entity that was already open.
    await page
      .locator(`[data-testid="inventory-neighbour"][data-entity-id="${GRAPH.neighbour.id}"]`)
      .click();

    await expect(detail).toContainText(GRAPH.neighbour.name, { timeout: 60_000 });
    await expect(page.getByTestId('inventory-entity-id')).toHaveText(GRAPH.neighbour.id);
    // Its file path, which only `get_entity` for THIS id can answer — the hub's
    // own read carries a different value for the same field.
    await expect(page.getByTestId('inventory-entity-file')).toHaveText(GRAPH.neighbour.filePath);
  });
});
