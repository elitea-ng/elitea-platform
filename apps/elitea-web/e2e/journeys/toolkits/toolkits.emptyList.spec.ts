/**
 * Journey 17.1 (JRNY-017) — an empty toolkit list redirects to the create page.
 *
 * ── WHY THIS IS A FILE OF ITS OWN, AND A PLAYWRIGHT PROJECT OF ITS OWN ─────
 *
 * The subject is `pages/toolkits/Toolkits.tsx`'s `shouldRedirectToCreatePage`
 * gate, and its input is the SERVER's row count for the shared project. That
 * makes "the list is empty" a property of the PLATFORM at one moment, not of
 * this test — and every other toolkit journey creates rows in that same
 * project while `fullyParallel` runs them all at once. `toolkits.catalogue
 * .spec.ts`'s J17C.1 holds one toolkit per served category, eight or more of
 * them, for the length of a test with an eleven-minute budget.
 *
 * Two things were tried and neither can work. Polling for a quiet moment
 * (what this journey used to do, with a 30 second budget) needs a window in
 * which no sibling holds a row, and on this tree there is none: the run
 * reported `expect(received).toBe(0)` received 8. A reader/writer lock like
 * `fixtures/platformFlags.ts`'s has the same shape of problem from the other
 * side — the writer would have to wait out that eleven-minute test.
 *
 * ORDER is what makes the precondition true. `playwright.config.ts` gives
 * this file its own project, `toolkits-empty`, and every engine project
 * depends on it, so it runs after `setup` and before the first journey that
 * can create a toolkit. Nothing is asserted more weakly: the count still
 * comes from the product's own GET, the redirect is still driven by that
 * response, and a platform that serves a non-empty list here still fails.
 *
 * It creates nothing itself, so it leaves the stack exactly as it found it
 * for the journeys that follow.
 */
import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { readsPlatformFlags } from '../../fixtures/platformFlags';

/*
 * The shared half of the platform-flag lock. The create screen this journey
 * lands on is `ToolkitTypeSelector`, which `useIsMcpVisible()` can take off
 * the page entirely, and `admin.features.spec.ts` turns that platform-wide
 * row off and back on (issue #519). This project runs before that journey
 * today; the lock is what keeps the statement true if the order ever changes.
 */
readsPlatformFlags(test);

/**
 * The create page's own discriminating landmark: ToolkitTypeSelector's
 * CategoryFilter search box. A stub route with a bare heading has no form
 * control; the placeholder text comes from
 * toolkits.toolkitTypeSelector.searchToolkit.
 */
function typeSearchBox(page: Page) {
  return page.getByPlaceholder('Search toolkits');
}

test('J17.1: an empty toolkit list redirects to the create page', async ({ page }) => {
  // Behaviour under test: Toolkits.tsx's shouldRedirectToCreatePage gate
  // (pages/toolkits/Toolkits.tsx:57-67) driven by the REAL list response
  // (GET /elitea_core/tools/prompt_lib/{id} -> total 0). A stub page cannot
  // redirect, because the redirect is a function of server data.
  /*
   * THE POLL IS KEPT, and it now guards something else.
   *
   * It was written for a race this project's ordering removes (issue #519: a
   * sibling journey's fixture existed while this one sampled, and the report
   * read as a broken redirect). What it still buys is the first sample of the
   * run against a stack that has just come up: the list is judged on a
   * response this test watched arrive, and a 200 that is slow to appear is
   * waited for rather than read as a non-empty project.
   *
   * Nothing here is weaker than the assertion it replaced: the count comes
   * from the product's own GET, the redirect is judged on THAT response, and
   * a platform that serves rows to this project fails on the count.
   */
  let listTotal = -1;
  await expect
    .poll(
      async () => {
        const listResponse = page.waitForResponse(
          (r) =>
            r.request().method() === 'GET' && /\/elitea_core\/tools\/prompt_lib\//.test(r.url()),
          { timeout: 20_000 },
        );
        await page.goto(BASE_URL + '/app/toolkits/all');
        const resp = await listResponse;
        expect(resp.status()).toBe(200);
        listTotal = ((await resp.json()) as { rows: unknown[]; total: number }).total;
        return listTotal;
      },
      { timeout: 30_000, intervals: [500, 1_000, 2_000] },
    )
    .toBe(0);
  expect(listTotal, 'the redirect below is judged on THIS response').toBe(0);

  await page.waitForURL(/\/app\/toolkits\/create/, { timeout: 20_000 });
  // The create screen really mounted — a form control, not a heading.
  await expect(typeSearchBox(page)).toBeVisible({ timeout: 15_000 });

  // This used to fail on defect A's fallout — two nameless ghost tiles tripping
  // axe's `button-name` rule. Both causes are fixed (see the file header and
  // J17.2's note), so checkA11y is now a clean, unconditional assertion.
  await checkA11y(page);
});
