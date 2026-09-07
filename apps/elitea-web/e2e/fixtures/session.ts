/**
 * Signing a browser in, for a journey that must own the session it drives.
 *
 * ## Why this exists
 *
 * `auth.setup.ts` mints ONE server-side session per persona and saves its
 * cookie to `.playwright-state/<persona>.json`. Every journey project loads
 * that file, so the whole suite — four workers, ~230 tests — presents the SAME
 * session identifier.
 *
 * That was harmless while the cookie was a self-contained signed token: a
 * logout could only delete the browser's own copy, and every other holder kept
 * working. Since shared migration 0117 the cookie names a ROW, and
 * `/forward-auth/logout` revokes it (`internal/api/v2/auth/session.go`). One
 * journey that signs out therefore signs out every worker, and from that
 * instant every later test presents a revoked identifier:
 *
 *   * an API helper gets `401 {"code":"session_revoked"}` — and it used to get
 *     `{"code":"unauthenticated","message":"missing authorization header"}`,
 *     the SAME body an unwired route answers, which is what made this cause
 *     take a separate investigation (#538). `describeRefusal` in
 *     `e2e/fixtures/api.ts` prints that code with this paragraph's summary;
 *   * a page load gets `401 session_expired` from the shell's session probe,
 *     and `app/session-probe-client.ts` navigates the tab to the identity
 *     provider, which surfaces as `net::ERR_ABORTED` or a `waitForURL`
 *     timeout in whatever journey happened to be navigating.
 *
 * So a journey whose SUBJECT is ending a session must not use the shared one.
 * It signs in here first, in a context it created itself, and destroys only
 * what it made. `scripts/e2e-journey-shape.test.mjs` holds that rule.
 */
import { expect, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';

/**
 * Drives the real OIDC authorization-code round trip for `email` and leaves
 * the browser signed in, exactly as `auth.setup.ts` does.
 *
 * The provider port is read from the environment for the same reason
 * `auth.setup.ts` reads it: `E2E_OIDC_PORT` moves the mock so a second stack
 * can run, and a hardcoded 9400 makes this wait out its whole timeout on a URL
 * that can never appear.
 */
export async function signInThroughOidc(page: Page, email: string): Promise<void> {
  await page.goto(BASE_URL + '/forward-auth/auth_oidc/login', { waitUntil: 'domcontentloaded' });
  await page.waitForURL(new RegExp(`oidc\\.localhost:${process.env['E2E_OIDC_PORT'] ?? '9400'}`), {
    timeout: 15_000,
  });
  await page.getByLabel('Subject').fill(email);
  await page.getByRole('button', { name: 'Authorize', exact: true }).click();
  await page.waitForURL(BASE_URL + '/**', { timeout: 15_000 });

  // The round trip must have produced a session, or every assertion the caller
  // then makes about signing OUT is satisfied by a browser that was never in.
  const info = await page.request.get(BASE_URL + '/forward-auth/info');
  expect(
    await info.json(),
    `the OIDC round trip did not establish a session for ${email}`,
  ).toMatchObject({ authenticated: true });
}
