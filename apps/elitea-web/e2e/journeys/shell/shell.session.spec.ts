/**
 * Journey 3: Session expiry mid-request → re-auth popup → retry (JRNY-003)
 * Journey 7: Project switch from the sidebar (JRNY-007)
 *
 * Spec §8.5 acceptance (from parity/manifest/shell.json JRNY-003/007).
 *
 * NOT TESTABLE ON THIS STACK — recorded here rather than faked anywhere
 * below: `deploy/docker-compose.e2e.yml:38` sets `VITE_SOCKET_SERVER: ""`,
 * so the app installs the noop socket client whose connection state is the
 * hardcoded literal `disconnected`. Any assertion about live socket
 * connection/reconnect behaviour (the sidebar's `sidebar-connection-dot`
 * included) would be asserting against a constant, not against the product.
 * Neither journey in this file depends on it.
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';

// ─────────────────────────────────────────────────────────────────────────────
// Journey 3: Session expiry → re-auth popup → retry
// ─────────────────────────────────────────────────────────────────────────────
test('J3: session expiry triggers re-auth popup and retries original request', async ({ page }) => {
  // JRNY-003, end to end against the real provider: a 401 mid-session must
  // open ONE re-auth popup, that popup must complete a real OIDC round trip,
  // and the original request must then succeed.
  //
  // What had to be wired for this to be reachable at all (issue #136 B), all
  // of it dead beforehand: `src/app/App.tsx` is the only bootstrap and called
  // `configureGeneratedClient({ baseUrl })` with no `reauthenticate`, so
  // `shared/api/http.ts`'s `runReauth()` returned false before doing anything
  // and `needsReauth()` was dead for every 401/403 in the app;
  // `createAuthPopupController` had no production call site; and the popup it
  // builds opened the callback route DIRECTLY, which cannot re-authenticate on
  // a stack that does not gate the SPA at the edge (the popup is simply served
  // the app, its session probe reports "no session", and the flight rejects).
  // The popup now opens `/forward-auth/auth_oidc/login` with the callback
  // route as `target_to`, which is the flow the assertions below drive.

  await page.goto(BASE_URL + '/app/');
  await page.waitForURL('**/chat**', { timeout: 15_000 });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 15_000 });

  // Expire the session for every subsequent API call.
  await page.route('**/api/v2/**', async (route) => {
    await route.fulfill({ status: 401, body: 'Unauthorized', contentType: 'text/plain' });
  });

  const popupPromise = page.waitForEvent('popup', { timeout: 15_000 });

  // Navigate somewhere that must fetch — the agents list issues an API call
  // on mount, which now 401s.
  await page.goto(BASE_URL + '/app/agents/all', { waitUntil: 'commit' });

  // 1. The re-auth popup must open, at the OIDC authorize endpoint.
  const popup = await popupPromise;
  // The provider's own authorize page, not merely "the popup went somewhere":
  // `oidc.localhost:<port>` is the alias elitea-main hands out (see
  // `deploy/docker-compose.e2e-standalone.yml`). A popup that stalled on the
  // app's own `/forward-auth/auth_oidc/login` hop — or on an error page,
  // which is what happened while the issuer was an unresolvable compose
  // hostname — does not match.
  //
  // The PORT comes from `E2E_OIDC_PORT`, as `auth.setup.ts:74` already does.
  // Hardcoding it here meant this was the one journey that could not run
  // against a second stack: `E2E_OIDC_PORT` moves the mock so two agents can
  // work at once, and this assertion then waited 15s for a URL that could
  // never appear. It failed on both engines and looked like a session defect.
  await popup.waitForURL(
    new RegExp(`oidc\\.localhost:${process.env['E2E_OIDC_PORT'] ?? '9400'}`),
    { timeout: 15_000 },
  );

  // 2. Completing re-auth in the popup must close it...
  await popup.getByLabel('Subject').fill('e2e-member@autotest.local');
  await popup.getByRole('button', { name: 'Authorize', exact: true }).click();
  await popup.waitForEvent('close', { timeout: 15_000 });

  // 3. ...and the original request must be retried and succeed. Stop
  //    401-ing so the retry can land, then assert the page recovered with
  //    real content rather than an error state.
  await page.unroute('**/api/v2/**');
  await expect(page.getByTestId('sidebar-create-button')).toBeEnabled({ timeout: 20_000 });
  await expect(page.getByText(/something went wrong|unauthorized/i)).toHaveCount(0);
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 7: Project switch from the sidebar
// ─────────────────────────────────────────────────────────────────────────────
/**
 * SECOND PROJECT IS A BACKEND STUB, DELIBERATELY — and it is the backend
 * that is stubbed, not the UI under test.
 *
 * The E2E seed contains exactly ONE project ("Default Project", id 1) for
 * both personas (measured: the switcher's listbox renders a single option
 * for `member` and for `admin`), and elitea-main exposes no way to make a
 * second one — `services/elitea-main/internal/api/v2/projects/handler.go`
 * serves `GET /api/v2/projects/project/default/{id}` and nothing else; there
 * is no create/delete project route to add a real `-shell` project with.
 * A switch journey against a one-item list asserts nothing, which is exactly
 * how the previous revision passed: it read `isVisible().catch(() => false)`,
 * then `if (!isVisible) return;`, then `if (count > 1)` — three nested
 * escapes, and on this stack `count` is 1, so the switch never ran.
 *
 * So the project LIST response gets a second row appended. Everything from
 * the switcher trigger inward is the real product: `ProjectSwitcher`'s
 * popper, `AppShell`'s `selectProject`, and
 * `widgets/app-shell/lib/selectedProjectPersistence.ts`'s `el.project.*`
 * writes. If any of those regress, this test fails.
 */
const SWITCH_TARGET_ID = '9901';
const SWITCH_TARGET_NAME = 'autotest_switch-shell';

test('J7: project switch from the sidebar', async ({ page }) => {
  // HOW MANY PROJECTS THE SERVER RETURNS IS NOT THIS TEST'S TO ASSUME.
  //
  // The subject is the switcher: it lists what the server sent plus the one
  // this test injects, and switching to that one persists. The count was
  // hardcoded to 2, which silently encoded "the member persona is in exactly
  // one project" — a property of the seed that this test never states and does
  // not own. Adding a seeded project for the DeepWiki journeys made it 3 and
  // this test reported a broken switcher.
  //
  // The count is now DERIVED from the response the switcher was given, so the
  // assertion still fails if the popper drops or duplicates an option, and no
  // longer fails because a sibling fixture exists.
  let serverProjectCount = -1;
  await page.route('**/api/v2/projects/project/default/**', async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as Array<Record<string, unknown>>;
    expect(Array.isArray(body), 'project list must be a bare array').toBe(true);
    expect(body.length, 'the seed must still contain Default Project').toBeGreaterThan(0);
    serverProjectCount = body.length;
    await route.fulfill({
      response,
      json: [...body, { ...body[0], id: Number(SWITCH_TARGET_ID), name: SWITCH_TARGET_NAME }],
    });
  });

  await page.goto(BASE_URL + '/app/');
  await page.waitForURL('**/chat**', { timeout: 15_000 });

  // The switcher trigger is the sidebar's "Project: <name>" button
  // (`widgets/sidebar/ui/ProjectSwitcher.tsx:66-88`). Asserting its
  // accessible name proves the selected project is really rendered, not just
  // that some button exists.
  const trigger = page.getByRole('button', { name: /Project:/ });
  await expect(trigger).toBeVisible({ timeout: 20_000 });
  // THE PERSONA'S LANDING PROJECT, ASSERTED AND NOT ARRANGED HERE. This is the
  // one place in the suite that reads back what `auth.setup.ts` pinned, so it
  // is deliberately left as an assertion: selecting the project first would
  // make it pass whatever the storage state carried. It failed on
  // "Project: Private" the moment sign-in began provisioning a personal
  // project for every account and the setup's pin became a race — which is
  // exactly the report this line is here to make.
  await expect(trigger).toHaveAccessibleName(/Project:\s*Default Project/, { timeout: 20_000 });

  await checkA11y(page);

  await trigger.click();

  // The popper is `open={open && projects.length > 0}` — a listbox with BOTH
  // projects in it, not merely "some listbox/dialog/menu appeared".
  const listbox = page.getByRole('listbox');
  await expect(listbox).toBeVisible({ timeout: 10_000 });
  // The interception really happened. Without this the expected count below
  // would be computed from -1 and the assertion would be meaningless.
  expect(serverProjectCount, 'the project list request was never intercepted').toBeGreaterThan(0);
  await expect(listbox.getByRole('option')).toHaveCount(serverProjectCount + 1);

  const target = listbox.getByRole('option', { name: SWITCH_TARGET_NAME });
  await expect(target).toHaveAttribute('aria-selected', 'false');
  await target.click();

  // 1. The switch is reflected in the trigger...
  await expect(trigger).toHaveAccessibleName(new RegExp(`Project:\\s*${SWITCH_TARGET_NAME}`), {
    timeout: 10_000,
  });
  // ...and the popper closed.
  await expect(listbox).toHaveCount(0);

  // 2. ...and persisted, in both storage areas
  //    (`selectedProjectPersistence.ts` writes local AND session).
  await expect
    .poll(() => page.evaluate(() => window.localStorage.getItem('el.project.id')), { timeout: 10_000 })
    .toBe(SWITCH_TARGET_ID);
  expect(await page.evaluate(() => window.localStorage.getItem('el.project.name'))).toBe(
    SWITCH_TARGET_NAME,
  );
  expect(await page.evaluate(() => window.sessionStorage.getItem('el.project.id'))).toBe(
    SWITCH_TARGET_ID,
  );

  // 3. ...and survives a reload, with the shell still mounted.
  await page.reload();
  await expect(page.getByTestId('sidebar-collapse-toggle')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole('button', { name: /Project:/ })).toHaveAccessibleName(
    new RegExp(`Project:\\s*${SWITCH_TARGET_NAME}`),
    { timeout: 20_000 },
  );

  // Leave the persona on the seeded project so a later spec sharing this
  // stack does not inherit the stubbed id.
  await page.evaluate(() => {
    for (const store of [window.localStorage, window.sessionStorage]) {
      store.removeItem('el.project.id');
      store.removeItem('el.project.name');
    }
  });
});
