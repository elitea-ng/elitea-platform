/**
 * Choosing the project a test runs in, through the product's own switcher.
 *
 * ## Why this is a fixture and not three copies
 *
 * Three places need the same thing, and each had its own version of it:
 * `auth.setup.ts` (which project a persona's storage state records),
 * `e2e/visual/lib/settle.ts` (which project a screenshot is taken in) and the
 * journeys that sign a fresh browser in and then assert on the shell. When
 * they disagreed, they disagreed silently — a suite whose shots and whose
 * journeys are about different projects still passes every assertion it makes.
 *
 * ## Why the switcher, and not `el.project.*`
 *
 * Writing the storage keys from a test races the app, which writes them
 * itself: `AppShell` selects a project when nothing is selected yet and
 * persists that choice (`widgets/app-shell/ui/AppShell.tsx`). Whichever write
 * lands last wins, and neither side can see the other. `e2e/visual/lib/
 * settle.ts` measured the same thing from the other end — an `addInitScript`
 * that wrote the keys left the switcher on the project it was trying to leave.
 *
 * The switcher is the only writer of those keys the product supports, journey
 * 7 covers it end to end, and a selection made through it is a selection the
 * app agrees it has made.
 */
import { expect, type Page } from '@playwright/test';

// The defaults and their parsing live in `scripts/lib` so they are unit-tested
// without Playwright — `scripts/e2e-live-assumptions.test.mjs`.
import { e2eTenancy, NO_PROJECT } from '../../scripts/lib/e2e-live-assumptions.mjs';

/* ── the tenancy a run targets ───────────────────────────────────────────────
 *
 * Every value below defaults to what `scripts/e2e-stack.sh` seeds, so a run
 * against the e2e rig sets none of them and behaves exactly as before. A run
 * against a DEPLOYED instance (`playwright.live.config.ts`) cannot reuse the
 * rig's tenancy — project 1 there holds real data, the regression personas
 * work in a project of their own, and the seeded publish/public projects do not
 * exist — so it names its own through these variables instead of editing
 * specs.
 *
 *   E2E_PROJECT_ID                 the project the personas work in
 *   E2E_DEFAULT_PROJECT_NAME       that project's name, as the switcher shows it
 *   E2E_PUBLIC_PROJECT_ID          the FRONTEND-public project (VITE_PUBLIC_PROJECT_ID)
 *   E2E_CATALOGUE_PROJECT_ID       the backend catalogue project (ELITEA_AI_PROJECT_ID)
 *   E2E_PUBLISH_AUTHOR_PROJECT_ID  the project publish journeys author in, or
 *                                  `none` when the deployment has no such project
 *
 * The resolvers that consume the last three live in `fixtures/api.ts`; the
 * live config itself is `playwright.live.config.ts`.
 */

const TENANCY = e2eTenancy(process.env);

/**
 * The project the personas work in. On the rig: the seeded shared project, id
 * 1, the project the seed fills, that both journey personas hold a role on, and
 * that every visual baseline naming no other project was taken in.
 */
export const DEFAULT_PROJECT_ID = TENANCY.projectId;
/** `DEFAULT_PROJECT_ID`'s name, exactly as the sidebar switcher lists it. */
export const DEFAULT_PROJECT_NAME = TENANCY.projectName;

/** `E2E_PUBLIC_PROJECT_ID`, when the run declares the frontend-public project by id. */
export const PUBLIC_PROJECT_ID_OVERRIDE = TENANCY.publicProjectId;
/** `E2E_CATALOGUE_PROJECT_ID`, when the run declares the catalogue project by id. */
export const CATALOGUE_PROJECT_ID_OVERRIDE = TENANCY.catalogueProjectId;
/**
 * `E2E_PUBLISH_AUTHOR_PROJECT_ID`: a project id, `NO_PROJECT` (`none`), or
 * undefined — resolve the seeded project by name, which is the rig's behaviour.
 */
export const PUBLISH_AUTHOR_PROJECT_ID_OVERRIDE = TENANCY.publishAuthorProjectId;
export { NO_PROJECT };

/** `RegExp` metacharacters in a project name, so a name is matched literally. */
function escapeForRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/** The switcher trigger's accessible name once `projectName` is selected, matched literally. */
export function switcherNameFor(projectName: string): RegExp {
  return new RegExp(`Project:\\s*${escapeForRegExp(projectName)}`);
}

/** The sidebar switcher's trigger — `widgets/sidebar/ui/ProjectSwitcher.tsx`. */
export function projectSwitcher(page: Page) {
  return page.getByRole('button', { name: /Project:/ });
}

/**
 * Waits until the shell has chosen a project of its own.
 *
 * The switcher renders whether or not a project is selected; its label is the
 * literal "No projects" until the project list resolves and something is
 * selected out of it. So a label that is NOT that literal is the evidence that
 * the app has made — and persisted — its own choice, which is what a caller
 * that is about to make a different choice has to wait for first.
 *
 * Thirty seconds, not the usual twenty: a first sign-in provisions a personal
 * project by applying a tenant migration corpus, and the shell polls
 * `/social/author` every three seconds while that runs
 * (`widgets/app-shell/model/usePersonalProjectId.ts`).
 */
export async function shellChoseAProject(page: Page): Promise<void> {
  const trigger = projectSwitcher(page);
  await expect(trigger).toBeVisible({ timeout: 30_000 });
  await expect(trigger).not.toHaveAccessibleName(/Project:\s*No projects/, { timeout: 30_000 });
}

/**
 * Leaves `projectName` selected, choosing it through the switcher if it is not
 * selected already.
 *
 * The already-selected case costs one `textContent()` read and no navigation,
 * which is the ordinary case once `auth.setup.ts` has pinned the persona's
 * project — so this is a guard against that pin drifting, not a page load per
 * call.
 */
export async function ensureProjectSelected(page: Page, projectName: string): Promise<void> {
  const trigger = projectSwitcher(page);
  await expect(trigger).toBeVisible({ timeout: 20_000 });

  // `textContent()` and not `toHaveAccessibleName()`: this is a QUESTION, not
  // an assertion, and a matcher would spend its whole timeout before answering
  // "no" on exactly the runs that need the switch.
  const label = (await trigger.textContent()) ?? '';
  if (label.includes(projectName)) return;

  await trigger.click();
  const listbox = page.getByRole('listbox');
  await expect(listbox).toBeVisible({ timeout: 10_000 });
  // The popper renders before the project list resolves — one disabled "No
  // projects" row — and fills in when the query answers, so waiting for the
  // option is also the wait for the list.
  const option = listbox.getByRole('option', { name: projectName });
  await expect(option, `the switcher must list ${projectName}`).toBeVisible({ timeout: 20_000 });
  await option.click();
  await expect(trigger).toHaveAccessibleName(switcherNameFor(projectName), { timeout: 20_000 });
}
