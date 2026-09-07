/**
 * Journey INV-010: the ask drawer answers a question through `investigate`,
 * and its citations are clickable.
 *
 * ## What this stack can and cannot prove
 *
 * `investigate` is declared on the `inventory_search` family and NOWHERE else,
 * so this journey is also the only assertion in the suite that the facade
 * routes a second family for the same toolkit. Sending it to `inventory` is an
 * unknown tool, refused as invalid input — which on screen is an error banner
 * rather than a wrong answer, so a mis-addressed drawer fails here loudly.
 *
 * What it cannot prove is that an AGENT reasoned: the fixture matches the
 * question against entity ids, names and file paths and reports what it hit.
 * The path around that — the invocation, the read-once progress events, the
 * `answer`/`entities` document, the citation click — is production code.
 *
 * ## Why the assertions are shaped this way
 *
 * "The drawer opened" and "a bubble appeared" are both true of a drawer whose
 * invocation 403'd, because the failure renders in the same transcript. So the
 * answer is asserted BY ITS CONTENT: the fixture echoes the question back
 * inside its answer, and lists the entities it matched by name and file path.
 * A fabricated or empty answer cannot contain either.
 *
 * The citations are asserted by COUNT and then by what clicking one OPENED. A
 * citation rendered as text rather than a link still reads correctly and does
 * nothing — the same defect class the graph journey's neighbour walk catches —
 * and the only way to tell is to click it and look at the entity pane.
 */
import { expect, test } from '@playwright/test';

import { STORAGE_STATE } from '../../../playwright.config';
import { openReadOnlyWorkspace, SEEDED } from './helpers';

/**
 * The question, and what the fixture's matcher does with it.
 *
 * "checkout" hits four of the six entities — three whose id or file path
 * carries it, and the guide named after it — and misses the two payment ones.
 * A question that matched everything would make the citation assertions
 * indistinguishable from "the drawer cited the whole graph".
 */
const QUESTION = 'checkout';
const CITED = [
  'code:checkout-service',
  'code:place-order',
  'code:order-model',
  'docs:checkout-guide',
];
/** The citation INV-010 follows, and the entity that must then be open. */
const FOLLOWED = SEEDED.graph.neighbour;

test.describe('Inventory ask', () => {
  test.use({ storageState: STORAGE_STATE.member });
  test.describe.configure({ timeout: 120_000 });

  test('INV-010: the ask drawer answers through investigate and cites entities', async ({
    page,
  }) => {
    await openReadOnlyWorkspace(page);

    await page.getByTestId('inventory-open-ask').click();
    const panel = page.getByTestId('inventory-ask-panel');
    await expect(panel).toBeVisible({ timeout: 30_000 });
    // Nothing has been asked, so the empty state is the honest screen — and its
    // presence here is what makes the answer below a change rather than a
    // constant.
    await expect(page.getByTestId('inventory-ask-empty')).toBeVisible();

    await page.getByTestId('inventory-ask-input').fill(QUESTION);
    await page.getByTestId('inventory-ask-send').click();

    const answer = page.getByTestId('inventory-ask-answer');
    await expect(answer).toBeVisible({ timeout: 90_000 });
    // The provider echoed the question back INSIDE the answer, so this rules
    // out a bubble rendered from the composer's own text.
    await expect(answer).toContainText(`Answer to "${QUESTION}"`);
    // …and named what it matched, by entity name and file path — values that
    // exist only in the graph the provider read.
    await expect(answer).toContainText('CheckoutService');
    await expect(answer).toContainText('src/checkout/service.py');
    // The two entities the matcher must MISS. An answer that cited the whole
    // graph would satisfy every assertion above.
    await expect(answer).not.toContainText('PaymentClient');

    // The citations, which are the ids the document carried in `entities`.
    const citations = page.getByTestId('inventory-ask-citation');
    await expect(citations).toHaveCount(CITED.length);
    for (const entityId of CITED) {
      await expect(citations.filter({ hasText: entityId })).toHaveCount(1);
    }

    // Following one. It must CLOSE the drawer and open the entity on the graph
    // tab — a citation that changed a selection behind a closed drawer, or on a
    // tab the user cannot see, reads as having done nothing.
    await page
      .locator(`[data-testid="inventory-ask-citation"][data-entity-id="${FOLLOWED.id}"]`)
      .click();

    await expect(panel).toBeHidden({ timeout: 30_000 });
    const detail = page.getByTestId('inventory-entity-detail');
    await expect(detail).toBeVisible({ timeout: 60_000 });
    await expect(page.getByTestId('inventory-entity-id')).toHaveText(FOLLOWED.id);
    await expect(detail).toContainText(FOLLOWED.name);
    // Read for THIS id: the file path is the entity's own and differs from
    // every other entity the answer cited.
    await expect(page.getByTestId('inventory-entity-file')).toHaveText(FOLLOWED.filePath);
  });
});
