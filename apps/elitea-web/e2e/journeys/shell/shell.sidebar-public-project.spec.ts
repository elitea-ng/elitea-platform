/**
 * Shell: the sidebar and Agents list on a PUBLIC project (issue #940 Bucket
 * D5, onetest ELITEA-1024 — sidebar-menu package).
 *
 * ── WHAT THE ONETEST CASE ASKED FOR, AND WHAT THIS PORTS BY USE CASE ───────
 *
 * ELITEA-1024's own claim is a PIXEL one: the Applications/Personalization/
 * Analytics/Environment nav rows should each draw an outlined, not filled,
 * icon, with Environment reachable only on a Public project. This app has no
 * `Personalization`/`Analytics`/`Environment` nav items at all —
 * `widgets/sidebar/lib/navSections.ts`'s `NavItemValue` union names seven
 * rows (chat/agents/pipelines/skills/toolkits/mcps/credentials/applications/
 * artifacts), none of them any of those three — so the case's literal
 * structure does not exist in this port, and a pixel-icon comparison belongs
 * to the VISUAL package regardless (issue #940's own D5 line says so).
 *
 * What DOES exist, and IS this port's real Public-project precondition, is
 * `navSections.ts`'s `computeIsSelectedProjectPublic` gate: it hides the
 * `skills` nav row on a public project (old app: `isSelectedProjectPublic`),
 * and the SAME id comparison (`entities/project`'s `isPublicProject`) makes
 * `pages/agents/Applications.tsx` render four PUBLIC tabs (Latest/My liked/
 * Trending/Admin) in place of the six PRIVATE ones. This file asserts THAT
 * rendered state — the shell's actual behavioural fork for "this project is
 * public" — rather than a pixel diff of an icon this app does not draw.
 *
 * ── WHY THIS NEEDED ITS OWN SEEDED PROJECT ──────────────────────────────────
 *
 * `isPublicProject` is a bare id comparison against `VITE_PUBLIC_PROJECT_ID`
 * (`deploy/docker-compose.e2e-standalone.yml` pins it to "99", DELIBERATELY
 * not "1" — see that file's own comment: pinning both broke every
 * agents/pipelines/skills journey by making the shared project 1 render as
 * the public view). That file's comment also states the gap this fills:
 * "99 is an id no fixture creates" — until `scripts/e2e-stack.sh seed` added
 * project 99 (this package), nothing on this stack ever satisfied that
 * comparison, so the switcher had no public-classified project to offer and
 * this precondition could not be reached at all.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { PUBLIC_PROJECT_NAME, resolvePublicProjectId } from '../../fixtures/api';
import { ensureProjectSelected } from '../../fixtures/project';

function rail(page: Page) {
  return page.getByRole('navigation', { name: 'side-bar' });
}

/* onetest: ELITEA-1024 — the Public-project precondition, ported by use case: the shell's actual public-project fork (Skills hidden, Agents shows public tabs), not a pixel icon diff (which belongs to the @visual package per issue #940's own D5 line) */
test('the sidebar and Agents list render the PUBLIC-project state, not the private one', async ({ page, request }) => {
  test.setTimeout(60_000);

  // Asserts the project resolves (the caller holds a role in it); id unused
  // beyond that — `ensureProjectSelected` switches by NAME, through the
  // product's own switcher, same as every other multi-project journey.
  await resolvePublicProjectId(request);

  await page.goto(BASE_URL + '/app/chat', { waitUntil: 'domcontentloaded' });
  await ensureProjectSelected(page, PUBLIC_PROJECT_NAME);

  // The Skills row is gone from the nav rail — `navSections.ts`'s
  // `computeIsSelectedProjectPublic` gate, the one real "public project"
  // branch this shell's sidebar has.
  await expect(rail(page).getByRole('link', { name: 'Skills', exact: true })).toHaveCount(0);

  // The rest of the rail still renders — this is a DIFFERENT state, not a
  // broken one.
  await expect(rail(page).getByRole('link', { name: 'Chats', exact: true })).toBeVisible();
  await expect(rail(page).getByRole('link', { name: 'Agents', exact: true })).toBeVisible();
  await expect(rail(page).getByRole('link', { name: 'Pipelines', exact: true })).toBeVisible();

  // Agents: the four PUBLIC tabs render (Latest/My liked/Trending), not the
  // six private ones (All/Drafts/Published/…) — `useApplicationTabs.tsx`'s
  // own split, gated on the exact same id comparison.
  await page.goto(BASE_URL + '/app/agents/all', { waitUntil: 'domcontentloaded' });
  // `Applications.tsx` redirects an invalid/private tab param to the first
  // PUBLIC tab on this project, replacing the URL — so landing on `latest`
  // is itself part of what this test is proving.
  await page.waitForURL('**/app/agents/latest**', { timeout: 15_000 });
  await expect(page.getByTestId('agents-tab-latest')).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId('agents-tab-my-liked')).toBeVisible();
  await expect(page.getByTestId('agents-tab-trending')).toBeVisible();
  // The private-only tabs must be absent, not merely unselected.
  await expect(page.getByTestId('agents-tab-all')).toHaveCount(0);
  await expect(page.getByTestId('agents-tab-drafts')).toHaveCount(0);
});
