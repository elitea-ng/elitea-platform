/**
 * Journey 5: Deep link to /agents/my/:id/:version cold-loads (JRNY-005)
 * Journey 6: Share link /{projectId}/agents/my/:id → project switch + reload (JRNY-006)
 *
 * Spec §8.5 acceptance (from parity/manifest/shell.json and agents.json).
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { createAgent, deleteAgent, DEFAULT_PROJECT_ID, AUTOTEST_PREFIX } from '../../fixtures/api';

/**
 * A deep link that renders the route error boundary is the failure mode
 * issue #132 produced: both API calls returned 200 and the page still said
 * "Something went wrong", because a paginated `{items,…}` envelope reached
 * code expecting an array. A URL check alone cannot see that, so both
 * journeys below assert the boundary is absent AND that real, data-bearing
 * controls rendered.
 */
const ERROR_BOUNDARY_TEXT = /something went wrong|unexpected error/i;

// ─────────────────────────────────────────────────────────────────────────────
// Journey 5: Deep link to an agent version cold-loads
// ─────────────────────────────────────────────────────────────────────────────
test('J5: deep link to specific agent version cold-loads', async ({ browser, request }) => {
  // Create an agent to deep-link into. No `.catch(() => null)` fallback: a
  // failure here is a real failure. The previous fallback branch navigated to
  // the plain agents list and asserted nothing about deep linking, so a
  // backend that could not create agents at all made this journey pass.
  const agentName = `${AUTOTEST_PREFIX}deeplink-shell`;
  const agent = await createAgent(request, agentName);

  try {
    // Use a fresh browser context to simulate a cold load.
    const context = await browser.newContext({ storageState: STORAGE_STATE.member });
    const page = await context.newPage();

    // Deep-link at the version the agent actually owns. `application_versions
    // .id` is a SERIAL, so the initial version is not reliably id 1 — and
    // `useIsVersionNotFound` (pages/agents/lib) compares the `:version`
    // segment against version IDs, rendering "Version not found" on a miss.
    const deepLink = `${BASE_URL}/app/agents/my/${agent.id}/${agent.versionId}`;
    await page.goto(deepLink, { waitUntil: 'domcontentloaded' });
    await page.waitForURL(`**/agents/my/${agent.id}/${agent.versionId}**`, { timeout: 15_000 });

    // Cold load means the page resolved the agent FROM THE URL ALONE. Prove
    // that with the editor's own populated form controls, not with
    // `getByRole('heading', { name: agentName })` as the previous revision
    // did: the agent name is also echoed into the `?name=` query parameter
    // and into the page title, so a heading carrying that text is far too
    // cheap a thing for a stub route to satisfy. A form INPUT whose *value*
    // came back from the detail fetch cannot be faked by a placeholder page.
    // The testid sits on the html input itself (`ApplicationEditForm.tsx:244`
    // passes it through `slotProps.htmlInput`), so this locator IS the control.
    const nameInput = page.getByTestId('agent-name-input');
    await expect(nameInput).toHaveValue(agentName, { timeout: 20_000 });

    // The description came from the same detail response (createAgent sends
    // `${AUTOTEST_PREFIX}e2e test agent`).
    const descriptionInput = page.getByTestId('agent-description-input');
    await expect(descriptionInput).toHaveValue(`${AUTOTEST_PREFIX}e2e test agent`);

    // The editor is live, not a read-only husk.
    await expect(page.getByTestId('agent-save-button')).toBeVisible();

    // NOT asserted on `edit-application-configuration-tab-panel`, which an
    // earlier revision of this test waited for: EditApplication.tsx renders
    // that testid as an empty `<Box/>` — a disclosed Wave-2 composition gap,
    // `Components/Applications/ConfigurationTab.jsx` was never ported — and an
    // empty element has zero size, so `toBeVisible()` can never pass on it.

    // Neither not-found state was hit: the agent AND the version both resolved.
    await expect(page.getByText(/agent not found|version not found/i)).toHaveCount(0);
    await expect(page.getByText(ERROR_BOUNDARY_TEXT)).toHaveCount(0);

    // The URL must be preserved (not redirected away).
    expect(page.url()).toContain(`/agents/my/${agent.id}/${agent.versionId}`);

    await checkA11y(page);
    await context.close();
  } finally {
    // No `.catch(() => {})`: `deleteAgent` does not throw on a non-2xx, so the
    // catch only ever hid a real transport failure.
    await deleteAgent(request, agent.id);
  }
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 6: Share link /{projectId}/agents/my/:id → project switch + reload
// ROUTE-070: the splat route ($projectId.$) strips the project segment and
// hard-reloads at the same path with the active project switched.
// ─────────────────────────────────────────────────────────────────────────────
// J6 uses the admin persona because the member persona is excluded from the
// project list when check_public_role=true and the user has no admin role.
// The admin (e2e-admin) has the 'admin' role on project 1 so they appear in
// ListCurrentUserProjects and the project switch redirect fires.
const j6Test = test.extend<object>({ storageState: STORAGE_STATE.admin });
j6Test('J6: share link with project id switches project and reloads', async ({ page }) => {
  // ROUTE-070: the /$projectId/$ splat strips the project segment and reloads.
  // Create an agent so the share link points at a real destination. No
  // fallback to `shareLink`: a creation failure is a real failure, and the
  // fallback previously let a backend with no applications routes at all
  // still satisfy this journey.
  const agentName = `${AUTOTEST_PREFIX}sharelink-shell`;
  const agent = await createAgent(page.request, agentName);
  const targetUrl = `${BASE_URL}/app/${DEFAULT_PROJECT_ID}/agents/my/${agent.id}`;

  try {
    await page.goto(targetUrl, { waitUntil: 'domcontentloaded' });

    // Wait for the STRIPPED path, not `**/agents/my/{id}**`. That looser
    // pattern also matches the pre-redirect `/app/1/agents/my/{id}` the
    // browser is already sitting on, so `waitForURL` returned immediately and
    // the assertion below raced ROUTE-070's `window.location.replace` instead
    // of waiting for it. (The redirect itself fires in well under a second —
    // measured against this stack; the failure was entirely in the wait.)
    await page.waitForURL(`**/app/agents/my/${agent.id}**`, { timeout: 15_000 });

    // The project segment should be stripped from the final URL.
    expect(page.url()).not.toContain(`/app/${DEFAULT_PROJECT_ID}/`);
    // ...and the destination the share link addressed is preserved.
    expect(page.url()).toContain(`/app/agents/my/${agent.id}`);

    // The hard reload must land on a WORKING page, not just a tidy URL. Same
    // reasoning as J5: a populated form control is something ROUTE-070's
    // `window.location.replace` target cannot produce unless the reload
    // really re-resolved the agent under the switched project.
    await expect(page.getByTestId('agent-name-input')).toHaveValue(agentName, {
      timeout: 20_000,
    });
    await expect(page.getByText(ERROR_BOUNDARY_TEXT)).toHaveCount(0);

    // ...and the switch actually took: the sidebar shows the project the
    // share link named. This persona reaches the page already pinned to that
    // project by `auth.setup.ts`, so what is really under test is that
    // ROUTE-070's reload did not LOSE it.
    await expect(page.getByRole('button', { name: /Project:/ })).toHaveAccessibleName(
      /Project:\s*Default Project/,
      { timeout: 20_000 },
    );

    await checkA11y(page);
  } finally {
    await deleteAgent(page.request, agent.id);
  }
});
