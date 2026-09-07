/**
 * Journey 1: Cold load / → redirect chain → /chat (JRNY-001)
 * Journey 2: Login via OIDC → target_to honoured (JRNY-002)
 * Journey 4: Logout → all el.* storage cleared (JRNY-004)
 *
 * Spec §8.5 acceptance (from parity/manifest/shell.json JRNY-001/002/004).
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { signInThroughOidc } from '../../fixtures/session';
import { BASE_URL } from '../../../playwright.config';

// ─────────────────────────────────────────────────────────────────────────────
// Journey 1: Cold load / → /chat
// ─────────────────────────────────────────────────────────────────────────────
test('J1: cold load / redirects through to /chat', async ({ page }) => {
  // Navigate to the root — auth setup already injected a valid session via
  // storageState, so elitea-main should honour the authenticated redirect chain.
  await page.goto(BASE_URL + '/');

  // The root performs a 302 redirect to /app/, which then redirects the
  // authenticated user through IndexRoute → /chat (ROUTE-003 + ROUTE-007).
  await page.waitForURL('**/chat**', { timeout: 15_000 });
  // Exact landing path, not a `**/chat**` glob: the glob also matches e.g.
  // `/app/chat-something` or a nested chat route.
  expect(new URL(page.url()).pathname).toBe('/app/chat');

  // NOT `getByTestId('chat-send-button').or(getByTestId('chat-input'))`. Two
  // problems with that: an `.or()` passes on whichever half exists, and the
  // whole disjunction only proved that SOME element carrying one of two
  // testids was on screen. The composer is the thing this journey is about,
  // so assert the composer itself — and then that it is a live, editable
  // control, which no stub/placeholder route can produce.
  const composer = page.getByTestId('chat-input');
  await expect(composer).toBeVisible({ timeout: 15_000 });

  // `features/chat-input/ui/UserInputEditableArea.tsx:96` wraps a real MUI
  // `TextField`; the inner textarea carries `chat-message-input` and the
  // placeholder `widgets/chat-box/ui/ChatBox.tsx` passes down.
  //
  // The copy is "Type your message...", not "Type a message...". That string
  // is not this journey's own decision: it is the value of the
  // `widgets.chatBox.inputPlaceholder` key in `shared/i18n/en.json`, and it
  // matches the parity baseline (`apps/elitea-ui`'s `NewChat.jsx:1336`,
  // `NewConversationView.jsx:943`) and the production screen the composer was
  // measured against. This test held the pre-parity copy and read a deliberate
  // correction as a regression; the copy was only ever a proxy for "this is
  // the product composer, not a stub", which the exact production string
  // states more strictly.
  const textarea = composer.getByTestId('chat-message-input');
  await expect(textarea).toBeEditable();
  await expect(textarea).toHaveAttribute('placeholder', 'Type your message...');

  // Typing must actually land in the composer — proves the chat feature is
  // mounted and interactive, not merely painted.
  await textarea.fill('probe-shell');
  await expect(textarea).toHaveValue('probe-shell');
  await textarea.fill('');

  await checkA11y(page);
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 2: Login via OIDC honours target_to
// ─────────────────────────────────────────────────────────────────────────────
test('J2: OIDC login honours target_to deep link', async ({ browser }) => {
  // Use a fresh context (no pre-loaded auth) to exercise the full login flow.
  const context = await browser.newContext({ storageState: undefined });
  const page = await context.newPage();

  // MEASURED PROPERTY OF THIS STACK (not an assumption): nginx serves the SPA
  // bundle for ANY /app/* path without an auth check, so navigating an
  // unauthenticated browser to a protected deep link does NOT produce an OIDC
  // redirect — the shell just loads with an empty session. The previous
  // revision of this test raced "did we get redirected to OIDC?" against "did
  // the agents page load?", and on this stack the second branch always won
  // instantly, so the OIDC half was dead and every assertion below it was
  // satisfied by an UNAUTHENTICATED page that happened to be at /agents. The
  // `.catch(() => null)` + `test.skip()` third branch then made even that
  // unreachable.
  //
  // What IS real, and is what JRNY-002 is actually about: elitea-main's
  // `/forward-auth/auth_oidc/login?target_to=<path>` encodes the target into
  // the OIDC `state` (`state=<nonce>|<target_to>`, verified against this
  // stack) and its callback lands the freshly-authenticated browser on that
  // path. That is the target_to contract; drive it end to end.
  const targetPath = '/app/artifacts';
  const loginResponse = await page.request.get(
    `${BASE_URL}/forward-auth/auth_oidc/login?target_to=${encodeURIComponent(targetPath)}`,
    { maxRedirects: 0 },
  );
  expect(loginResponse.status()).toBe(302);

  const location = loginResponse.headers()['location'];
  expect(location, 'login endpoint must 302 to the OIDC authorize endpoint').toBeTruthy();
  // The target must survive into the OIDC `state` — if elitea-main dropped it
  // here, no amount of post-login URL checking could tell us why.
  expect(decodeURIComponent(location!)).toContain(targetPath);

  // No hostname rewrite: `deploy/docker-compose.e2e-standalone.yml` gives the
  // provider the network alias `oidc.localhost`, which resolves to this
  // container from inside the compose network AND to loopback from the host
  // browser, so the authorize URL elitea-main hands out is navigable as-is.
  // (It used to be `oidc-mock:9400`, a compose-internal name only reachable
  // from inside, which every test that drove the login endpoint itself had to
  // string-rewrite — and which J3's browser-initiated popup navigation could
  // not rewrite at all.)
  await page.goto(location!, { waitUntil: 'domcontentloaded' });
  await page.getByLabel('Subject').fill('e2e-member@autotest.local');
  await page.getByRole('button', { name: 'Authorize', exact: true }).click();

  // After the callback the user must land on the originally requested path.
  await page.waitForURL(`**${targetPath}**`, { timeout: 30_000 });
  expect(new URL(page.url()).pathname).toBe(targetPath);
  // The auth_state redirect parameter must be stripped from the final URL.
  expect(page.url()).not.toContain('auth_state');

  // ...and the session the round trip established must be real. Without this
  // the URL assertions above would also pass for an unauthenticated shell
  // that was simply served the bundle at that path (which is exactly what
  // this stack does — see the note above).
  await expect(page.getByRole('button', { name: /Project:\s*Default Project/ })).toBeVisible({
    timeout: 20_000,
  });

  await checkA11y(page);
  await context.close();
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 4: Logout → all el.* storage cleared
// ─────────────────────────────────────────────────────────────────────────────
test('J4: logout clears user state and el.* storage', async ({ browser }) => {
  // THIS JOURNEY OWNS THE SESSION IT DESTROYS — read this before replacing the
  // context below with the shared `page` fixture it used to take.
  //
  // Since shared migration 0117 the `elitea_session` cookie names a ROW and
  // `/forward-auth/logout` REVOKES it (`internal/api/v2/auth/session.go`).
  // `auth.setup.ts` mints one session per persona and all four workers replay
  // its cookie, so a logout driven on the shared state signs out the whole
  // suite: every later API helper gets `401 missing authorization header` and
  // every later page load is navigated to the identity provider by the shell's
  // session probe. Measured on the 1.60.0 smoke run — 21 chromium journeys
  // failed downstream of this one test, none of them for a reason of their
  // own. See `e2e/fixtures/session.ts`.
  //
  // JRNY-004's three acceptance lines, each asserted separately below:
  // the user state is cleared, the login screen is reached, and all
  // application storage keys are removed.
  //
  // ASSERTION CHANGED FROM `waitForURL` TO NAVIGATION REQUESTS — read this
  // before "simplifying" it back. The earlier revision asserted
  //
  //   await page.waitForURL(/localhost:9400|oidc-mock|\/forward-auth\/logout/)
  //
  // and that matcher cannot express what logout does, for a reason measured
  // directly rather than assumed. `performLogout()` sends the browser to
  // `/forward-auth/logout?target_to=/forward-auth/login`, which elitea-main
  // answers 302 → `/forward-auth/login` → 302 `/forward-auth/auth_oidc/login`,
  // itself 302 → the provider's `/oauth2/authorize`. All hops were observed as real
  // navigation requests — but a chain of server-side 302s COMMITS exactly one
  // document, so `page.url()` and `framenavigated` only ever report the final
  // landing and both intermediate hops are invisible to a URL matcher.
  // Measured: with the old bare-logout hand-off the only URL ever reported
  // was `/app/`.
  //
  // So the URL matcher is replaced by an assertion on the navigation requests
  // themselves, which is STRICTLY STRONGER than the glob it replaces: it
  // proves the browser left the SPA for the logout endpoint, that the hand-off
  // named the login screen, and that the chain then reached the identity
  // provider's authorize endpoint, IN ORDER — none of which the old glob
  // distinguished (its third alternative, `/forward-auth/logout`, would also
  // have matched a chain that got no further). Step 2 additionally asserts the
  // SERVER session is really gone, which the previous revision never checked
  // at all.
  // Settings › PROFILE, not Personalization. "Log out" used to be a NAV ITEM
  // in the drawer's PERSONAL group, and this journey opened whichever settings
  // screen was cheapest because the drawer carried the control on every one of
  // them. It is now a BUTTON on the Profile page, under the identity rows
  // (`features/settings/ui/profile/ProfileIdentity.tsx`), which is where the
  // parity baseline and the production UI put it — an action stopped
  // pretending to be a tab. `src/routes/__tests__/settingsLogout.test.tsx`
  // covers the storage sweep in jsdom and names THIS journey as the half that
  // proves the browser really leaves the app, so the page it opens has to be
  // the page the control is on.
  const context = await browser.newContext({ storageState: undefined });
  const page = await context.newPage();
  await signInThroughOidc(page, 'e2e-member@autotest.local');

  await page.goto(BASE_URL + '/app/settings/profile', { waitUntil: 'domcontentloaded' });

  // Wait for the logout control to be interactive before touching storage.
  const logoutItem = page.getByRole('button', { name: 'Log out', exact: true });
  await expect(logoutItem).toBeVisible({ timeout: 20_000 });

  // Precondition: the session this journey is about to destroy really exists.
  // Without this, every assertion below is also satisfied by a browser that
  // was never signed in.
  const before = await page.request.get(BASE_URL + '/forward-auth/info');
  expect(await before.json()).toMatchObject({ authenticated: true });

  // A sentinel in the namespace performLogout() is contracted to sweep, plus
  // the two keys the app itself writes (`el.project.id` / `el.project.name`,
  // widgets/app-shell/lib/selectedProjectPersistence.ts).
  //
  // The CONTROL key is outside the namespace, and it is what makes step 3
  // below a measurement rather than a formality. Step 3 reads the storage from
  // a document that is not the app (see its note). A probe that read the wrong
  // origin, or an empty store, would report "no `el.` keys survive" and pass
  // while proving nothing. The control key must survive the logout, so a
  // vacuous read fails. It also proves the sweep is scoped to the namespace
  // and is not a blanket `clear()`.
  await page.evaluate(() => {
    for (const store of [window.localStorage, window.sessionStorage]) {
      store.setItem('el.test-sentinel-shell', '1');
      store.setItem('e2e-logout-control', '1');
    }
  });

  const navigations: string[] = [];
  page.on('request', (request) => {
    if (request.isNavigationRequest()) navigations.push(request.url());
  });

  await logoutItem.click();

  // 1. Logout must leave the SPA for the backend logout endpoint, and that
  //    hand-off must carry the browser on to the identity provider.
  await expect
    .poll(() => navigations.filter((url) => /\/oauth2\/authorize|\/authorize\?/.test(url)).length, {
      timeout: 15_000,
    })
    .toBeGreaterThan(0);
  const logoutHop = navigations.find((url) => url.includes('/forward-auth/logout'));
  expect(logoutHop, 'the browser must navigate to /forward-auth/logout').toBeTruthy();
  // The hand-off must name the login screen as its target, or the signed-out
  // user is parked on the index route's loading state instead.
  //
  // The target is `/forward-auth/login`, NOT `/forward-auth/auth_oidc/login`.
  // Only `/forward-auth/login` is registered on both authentication planes.
  // The OIDC-only path ended a form-plane logout on a bare 404. THIS stack is
  // the OIDC plane. On it, `/forward-auth/login` answers 302 to
  // `/forward-auth/auth_oidc/login`. The chain below is therefore one hop
  // longer. Its end state is the same.
  expect(new URL(logoutHop!).searchParams.get('target_to')).toBe('/forward-auth/login');
  // ...and the login hops must sit BETWEEN the logout and the provider.
  const order = (predicate: (url: string) => boolean): number => navigations.findIndex(predicate);
  expect(order((url) => url.includes('/forward-auth/login'))).toBeGreaterThan(
    order((url) => url.includes('/forward-auth/logout')),
  );
  expect(order((url) => url.includes('/forward-auth/auth_oidc/login'))).toBeGreaterThan(
    order((url) => url.includes('/forward-auth/login')),
  );

  // 2. The SERVER session must be gone — the "user state is cleared" half.
  //    A client-side storage sweep alone would leave the user still signed in.
  const after = await page.request.get(BASE_URL + '/forward-auth/info');
  expect(await after.json()).toMatchObject({ authenticated: false });

  // 3. ...and it must have swept the namespace on the way out.
  //
  // THE READ MUST NOT START A NEW AUTHENTICATION FLIGHT — issue #482. Read
  // this before you replace the probe below with a plain `goto('/app/')`.
  //
  // Web storage is per ORIGIN, and the logout chain parks the tab on the
  // identity provider's origin, so the read has to come back to the app
  // origin. Web storage is also per TAB for the session area, so the read has
  // to happen in THIS tab: a second page of the same context gets its own
  // sessionStorage and could never see the sentinel written above.
  //
  // The earlier revision came back with `page.goto(BASE_URL + '/app/')`. That
  // path serves the SPA (measured, and stated in J2's note above), the SPA
  // finds no session, and its re-auth controller starts a NEW flight, which
  // writes `el.auth.state` and `el.auth.flight.started`
  // (`shared/api/auth/constants.ts`). Those are two of the keys this assertion
  // then looks for, so the last step of the test caused the condition the test
  // measures. Measured: one tree, two runs, opposite results, and exactly
  // those two keys surviving in the failure.
  //
  // A retry or a longer timeout cannot correct that, because the keys are not
  // late — they are new. Waiting for the new flight to settle and then
  // ignoring its two keys is worse: it stops the test proving that logout
  // clears `el.auth.state`, which is one of the keys the test exists to check.
  //
  // So the tab returns to the app ORIGIN in a document that is not the app.
  // The probe response is served by the test, so no bundle loads, nothing
  // probes the session, and no flight can start. The URL decides the origin,
  // and the origin decides which storage the read sees, so both areas below
  // are the application's — asserted, not assumed, on the two lines that read
  // `location.origin` and the control key.
  //
  // The probe then found a SECOND writer, which is a product defect and is
  // fixed in `shared/api/auth/logout.ts` rather than accommodated here: the
  // logging-out document lives through the whole redirect chain, its open
  // requests 401 once the cookie is cleared, and `runReauth()` started a
  // flight that put the same two keys back. 2 failures in 60 WebKit runs.
  // `performLogout()` now marks the document, and the popup controller
  // refuses a flight while that mark is set.
  const probeUrl = BASE_URL + '/app/e2e-storage-probe';
  await page.route(probeUrl, (route) =>
    route.fulfill({
      status: 200,
      contentType: 'text/html',
      body: '<!doctype html><title>storage probe</title>',
    }),
  );
  await page.goto(probeUrl, { waitUntil: 'domcontentloaded' });

  const probe = await page.evaluate(() => {
    const surviving: string[] = [];
    const control: string[] = [];
    for (const store of [window.localStorage, window.sessionStorage]) {
      for (let i = 0; i < store.length; i++) {
        const key = store.key(i);
        if (key === null) continue;
        if (key.startsWith('el.')) surviving.push(key);
        if (key === 'e2e-logout-control') control.push(key);
      }
    }
    return { origin: window.location.origin, surviving, control };
  });
  await page.unroute(probeUrl);

  // The read happened on the application's origin...
  expect(probe.origin).toBe(new URL(BASE_URL).origin);
  // ...and it really saw the store the application wrote to. Both areas still
  // hold the control key, so an empty `surviving` list is a measurement and
  // not an artefact of reading the wrong place.
  expect(probe.control).toHaveLength(2);
  // ...and no key of the namespace survived the logout.
  expect(probe.surviving).toEqual([]);

  await context.close();
});
