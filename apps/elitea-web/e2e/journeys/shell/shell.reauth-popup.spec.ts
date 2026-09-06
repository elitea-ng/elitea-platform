/**
 * Re-auth popup regression (issue #364).
 *
 * J3 in `shell.session.spec.ts` already drives this flow, but it catches the
 * defect only on a race: it fails when the second flight lands BETWEEN the
 * fill and the click, which happened on 6 of 15 WebKit legs in CI and on 0 of
 * 10 locally. A gate that fires 40 percent of the time is not a gate, and the
 * retry bucket turned those failures green.
 *
 * This test removes the race from the assertion. It pins the property the
 * user cares about — the popup they are typing in is never taken over — and
 * it reads the same on every engine and every machine.
 *
 * Measured mechanism, from an instrumented WebKit run: `page.goto` below is a
 * full document load. The single-flight guard of `createAuthPopupController`
 * is closure state, so it dies with the document. The new document built a
 * second controller, saw an empty slot, and started a second flight into the
 * popup that was already open. On the base branch that reproduced on 4 of 4
 * runs, with the exact signature issue #364 reports: one popup page, two
 * `auth_state` values.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

const TYPED = 'e2e-member@autotest.local';

/**
 * The `auth_state` a URL carries, or `null`. The provider holds it one level
 * in, inside the `<nonce>|<target_to>` OIDC `state` that elitea-main builds,
 * so read it out of the whole URL rather than out of a named parameter.
 */
function authStateOf(url: string): string | null {
  const match = /auth_state(?:=|%3D)([0-9a-f-]+)/i.exec(url);
  return match?.[1] ?? null;
}

test('a page load must not restart the re-auth flight in the open popup', async ({
  page,
  context,
}) => {
  /*
   * THE BUDGET, which the default 30s never covered.
   *
   * This test waits, in order, for a route load, a client redirect (15s), a
   * composer (15s), a popup (15s), that popup reaching the provider (15s),
   * and then sleeps a mandatory 3s over a second document load. That is more
   * than 30s of allowance for six steps sharing one 30s test. It passed only
   * while every step was fast, and on webkit under four parallel workers the
   * FIRST navigation alone spent the whole budget — run 34016199152 reports
   * this test flaky, failing attempt 1 at the `goto` below and passing on
   * retry, while chromium passed outright. Nothing in the re-auth flow was
   * reached on the attempt that failed, so nothing about it was under test.
   *
   * The timeout is raised to fit the steps the test actually performs. It is
   * not a fix for a slow product: each individual wait keeps its own tight
   * bound, so a real regression still fails at the step that regressed
   * instead of expiring the whole test somewhere earlier.
   */
  test.setTimeout(120_000);

  /*
   * Every distinct `auth_state` that ever reaches a popup of this tab.
   *
   * Counting states rather than reading the current URL is what makes the
   * assertion deterministic. The takeover completes in well under a second,
   * so a later read of `popup.url()` already shows the SECOND state and looks
   * healthy — that is precisely how this defect stayed invisible.
   */
  const flights: string[] = [];
  const record = (url: string): void => {
    const state = authStateOf(url);
    if (state !== null && !flights.includes(state)) flights.push(state);
  };
  context.on('page', (opened) => {
    // The page may already carry its first URL by the time this handler runs,
    // so read it here as well as on every later navigation.
    record(opened.url());
    opened.on('framenavigated', (frame) => {
      if (frame === opened.mainFrame()) record(frame.url());
    });
  });

  /*
   * `domcontentloaded`, not the default `load`. The two assertions under this
   * line are the readiness this test needs, and they are stronger than the
   * `load` event: the app has to redirect to `/chat` and mount a composer.
   * `load` additionally waits for every subresource of the SPA document,
   * which on webkit is where the lost budget went — and satisfying it proves
   * nothing this test goes on to use. `auth.setup.ts` and `visual/lib/
   * settle.ts` open the same route the same way.
   */
  await page.goto(BASE_URL + '/app/', { waitUntil: 'domcontentloaded' });
  await page.waitForURL('**/chat**', { timeout: 15_000 });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 15_000 });

  // Expire the session for every subsequent API call, exactly as J3 does.
  await page.route('**/api/v2/**', async (route) => {
    await route.fulfill({ status: 401, body: 'Unauthorized', contentType: 'text/plain' });
  });

  const popupPromise = page.waitForEvent('popup', { timeout: 15_000 });
  // A FULL document load, which is what discards the guard.
  await page.goto(BASE_URL + '/app/agents/all', { waitUntil: 'commit' });

  const popup = await popupPromise;
  await popup.waitForURL(
    new RegExp(`oidc\\.localhost:${process.env['E2E_OIDC_PORT'] ?? '9400'}`),
    { timeout: 15_000 },
  );
  record(popup.url()); // the flight this popup opened with
  await popup.getByLabel('Subject').fill(TYPED);

  /*
   * Load a new document over the tab while the user types in the popup.
   *
   * This is the step that makes the assertion deterministic instead of racy.
   * The `goto` above already loads a document, but whether it does so BEFORE
   * or AFTER the popup opens is a race, and on the runs where it wins there
   * is only ever one flight and nothing to detect. A reload always lands
   * after the popup, so the guard is always discarded with a popup open.
   *
   * A user reaches the same state by pressing reload, or by opening a
   * bookmark, while the re-auth popup waits for them.
   */
  await page.reload({ waitUntil: 'commit' });

  // The new document mounts and issues its own 401s over this window. Three
  // seconds is ten times the 0.3 s poll and four times the 0.8 s gap the
  // issue measured between the two authorize hops.
  await page.waitForTimeout(3_000);

  // 1. Exactly ONE re-auth flight ever touched this tab. A second flight
  //    either replaces this page, which takes the typed value with it, or
  //    opens a second popup, which invalidates the first login at the
  //    provider. Both leave the user stuck.
  expect(
    flights,
    `a second re-auth flight reached the open popup: ${flights.join(', ')}`,
  ).toHaveLength(1);
  // 2. The typed value survives, which is the user-visible harm in #364.
  expect(await popup.getByLabel('Subject').inputValue()).toBe(TYPED);
  // 3. One tab and one popup.
  expect(context.pages().length, 'a second re-auth popup opened').toBe(2);
});
