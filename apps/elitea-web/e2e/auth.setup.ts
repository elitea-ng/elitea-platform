/**
 * Global auth setup (issue #60, unit V1 spec §OIDC).
 *
 * Performs a real OIDC authorization-code round trip via oidc-provider-mock:
 *   1. Navigate to the app → elitea-main redirects to the mock's authorize endpoint.
 *   2. Fill the "Subject" field with the persona's email and click "Authorize".
 *   3. The mock redirects back through /forward-auth/auth_oidc/callback →
 *      elitea-main sets the session cookie → app loads.
 *   4. Save storageState per persona so the 30 journey specs reuse the
 *      authenticated session without re-logging in for every test.
 *
 * The vite_dev_token bypass was deliberately removed (spec C7b, waiver W-001);
 * this is the only way E2E tests can authenticate.
 */
import { mkdirSync } from 'fs';
import * as path from 'path';

import { test as setup, expect } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../playwright.config';
import {
  DEFAULT_PROJECT_ID,
  DEFAULT_PROJECT_NAME,
  ensureProjectSelected,
  shellChoseAProject,
} from './fixtures/project';

setup.describe('Auth setup', () => {
  setup.beforeAll(() => {
    // Ensure the state directory exists.
    const dir = path.dirname(STORAGE_STATE.member);
    mkdirSync(dir, { recursive: true });
  });

  setup('authenticate as member persona', async ({ page }) => {
    setup.setTimeout(90_000);
    await performOidcLogin(page, 'e2e-member@autotest.local', STORAGE_STATE.member, 'seeded');
  });

  setup('authenticate as admin persona', async ({ page }) => {
    setup.setTimeout(90_000);
    await performOidcLogin(page, 'e2e-admin@autotest.local', STORAGE_STATE.admin, 'seeded');
  });

  // The #284 chat driver. Its personal project is what the /llm hop resolves
  // the provider credential from, which is why it cannot be one of the two
  // personas above — see `playwright.config.ts`'s STORAGE_STATE.chat.
  setup('authenticate as chat-driver persona', async ({ page }) => {
    setup.setTimeout(90_000);
    await performOidcLogin(page, 'e2e-chat@autotest.local', STORAGE_STATE.chat, 'personal');
  });
});

/**
 * Drives a full OIDC authorization-code flow via oidc-provider-mock.
 *
 * The mock's authorize screen shows a simple form with a "Subject" text input
 * and an "Authorize" button (no password). Filling the email and clicking
 * Authorize completes the flow.
 */
async function performOidcLogin(
  page: import('@playwright/test').Page,
  email: string,
  storageStatePath: string,
  project: PersonaProject,
): Promise<void> {
  // Navigate to the OIDC login endpoint — elitea-main redirects to the mock's
  // authorize page (the SPA does no automatic server redirect of its own).
  //
  // No hostname rewrite any more. `OIDC_ISSUER_URL` used to be the
  // compose-internal `http://oidc-mock:9400`, so the authorize URL
  // elitea-main handed out was unreachable from the host browser and this
  // setup had to read the `Location` header itself and rewrite the host. It is
  // `http://oidc.localhost:${E2E_OIDC_PORT}` now — a network alias on the oidc-mock
  // service that resolves to the container inside the compose network and to
  // loopback from the host (see `deploy/docker-compose.e2e-standalone.yml`) —
  // so a plain navigation follows the whole chain. That change is what made
  // J3's re-auth popup possible at all: a redirect the BROWSER follows on its
  // own cannot be rewritten from the test side.
  await page.goto(BASE_URL + '/forward-auth/auth_oidc/login', {
    waitUntil: 'domcontentloaded',
  });

  // Wait for the OIDC mock's authorize page.
  //
  // The port is read from the environment rather than hardcoded so a second
  // stack (E2E_OIDC_PORT) can be driven by the same suite; the default is the
  // 9400 every existing invocation already uses.
  await page.waitForURL(new RegExp(`oidc\\.localhost:${process.env['E2E_OIDC_PORT'] ?? '9400'}`), {
    timeout: 15_000,
  });

  // oidc-provider-mock authorize form: fill Subject (the user's email) and submit.
  // The field label is "Subject" per the mock's default template.
  await page.getByLabel('Subject').fill(email);
  await page.getByRole('button', { name: 'Authorize', exact: true }).click();

  // The mock redirects back through elitea-main's callback handler, which sets
  // the session cookie and ultimately lands the user on the app.
  await page.waitForURL(BASE_URL + '/**', { timeout: 15_000 });

  // Verify session is valid by calling the session info endpoint directly.
  // The SPA router context is not yet wired to a session store (Wave-2 gap),
  // so we cannot rely on sidebar-toggle rendering; instead, /forward-auth/info
  // confirms the server-side cookie round-trip succeeded.
  const infoResponse = await page.request.get(BASE_URL + '/forward-auth/info');
  const infoBody = (await infoResponse.json()) as { authenticated?: boolean; user_id?: string };
  expect(
    infoBody,
    `OIDC session not authenticated for ${email}; info: ${JSON.stringify(infoBody)}`,
  ).toMatchObject({ authenticated: true });
  // A session with no subject would still satisfy `authenticated: true` but
  // leaves every downstream journey without an identity to assert against.
  expect(infoBody.user_id, `OIDC session for ${email} carries no user_id`).toBeTruthy();

  // Open the app on the BASE_URL origin — both because the storage this
  // function is about belongs to that origin, and because the SPA may still be
  // completing the redirect that followed the callback.
  await page.goto(BASE_URL + '/app/', { waitUntil: 'domcontentloaded', timeout: 20_000 });

  // ── PIN THE PERSONA'S PROJECT, THROUGH THE PRODUCT'S OWN SWITCHER ────────
  //
  // WHAT WENT WRONG WITHOUT THIS. The block that stood here read
  // `GET /social/author`, took `personal_project_id ?? '1'`, and wrote the
  // answer straight into both storage areas. Two races decided what it wrote,
  // and each one had a different answer per continuous-integration job:
  //
  //   1. `GET /social/author` provisions the personal project and waits at
  //      most three seconds for it (`defaultPersonalProjectWait`,
  //      services/elitea-main/internal/api/v2/social/handler.go). Sign-in
  //      itself now starts the same work (`ensurePersonalProject`,
  //      internal/api/v2/auth/signin.go), so this read answers the new
  //      project's id when provisioning is quick and `""` when it is not. The
  //      persona was therefore pinned to project 1 on one job and to its own
  //      personal project on the next, out of the same code.
  //   2. The `/app/` visit above boots the shell, which selects a project of
  //      its own and PERSISTS it. That write lands whenever the two lists it
  //      waits on are answered — before or after the write here, unpredictably
  //      — so the storage state could carry the app's choice rather than ours.
  //
  // Measured on the 1.60.0 smoke run: the `E2E (chromium)` job recorded
  // project 1 for the member persona and its personal project for the admin
  // one, while both `E2E (webkit)` shards and the visual job recorded the
  // personal project for the member persona. Fifty journeys and forty-three
  // screenshots then failed on the project they were taken in, in three jobs
  // out of four, from one commit.
  //
  // The fix is to stop guessing. The shell is allowed to settle first — its
  // own write happens BEFORE anything here — and then the project this persona
  // is supposed to work in is chosen through the switcher, the way a user
  // chooses it, which is also the only writer of `el.project.*` the app
  // supports. What the recorded state carries is then an assertion, not a race.
  await shellChoseAProject(page);

  if (project === 'seeded') {
    await ensureProjectSelected(page, DEFAULT_PROJECT_NAME);
    const stored = await page.evaluate(() => localStorage.getItem('el.project.id'));
    expect(stored, `${email} must be pinned to the seeded project`).toBe(DEFAULT_PROJECT_ID);
  } else {
    // The chat driver works INSIDE its personal project — that is what the
    // `/llm` hop resolves the provider credential from (#290), and it is why
    // this persona exists at all. So the pin is its personal project, and the
    // one thing worth asserting is that the shell really settled there rather
    // than on the shared project every other persona uses.
    const personalProjectId = await readPersonalProjectId(page);
    expect(personalProjectId, `${email} must own a personal project`).toBeTruthy();
    await expect
      .poll(() => page.evaluate(() => localStorage.getItem('el.project.id')), { timeout: 30_000 })
      .toBe(personalProjectId);
  }

  // Save the authenticated state (cookies + localStorage).
  await page.context().storageState({ path: storageStatePath });
}

/**
 * Which project a persona is recorded in.
 *
 *  - `seeded`   — project 1, "Default Project": the project the seed fills and
 *                 that every journey and every visual baseline is written
 *                 against.
 *  - `personal` — the caller's own `project_user_<uid>`. Only the chat driver
 *                 wants this; see `playwright.config.ts`'s STORAGE_STATE.chat.
 */
type PersonaProject = 'seeded' | 'personal';

/**
 * The caller's `personal_project_id`, asked for until the server names one.
 *
 * The same poll the shell itself runs (`widgets/app-shell/model/
 * usePersonalProjectId.ts`): `""` is not an error and not a terminal answer on
 * a first sign-in, it means provisioning has not finished, and the only way to
 * learn that it changed is to ask again.
 */
async function readPersonalProjectId(
  page: import('@playwright/test').Page,
): Promise<string | undefined> {
  let id: string | undefined;
  await expect
    .poll(
      async () => {
        const author = await page.request.get(BASE_URL + '/api/v2/social/author/');
        if (!author.ok()) return '';
        id = ((await author.json()) as { personal_project_id?: string }).personal_project_id;
        return id ?? '';
      },
      { timeout: 30_000 },
    )
    .not.toBe('');
  return id;
}
