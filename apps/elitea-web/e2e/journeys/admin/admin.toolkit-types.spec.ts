/**
 * JRNY-038 — `Admin › Toolkits`: deciding which toolkit types this platform
 * offers, and to which projects (shared migration 0114).
 *
 * ## What would be green without this
 *
 * Every unit test in `ToolkitTypes.test.tsx` drives the page against mocked
 * HTTP. They prove the page sends the right request. They cannot prove that the
 * decision reaches the SERVED CATALOGUE — the map the "+ Toolkit" chooser reads
 * — because that is a different route, in a different package, filtered by a
 * different composition. That join is exactly the class this repository keeps
 * finding: both halves correct, the wiring absent.
 *
 * So each write below is followed by a read of
 * `GET /elitea_core/toolkits/prompt_lib/{projectID}`, which is the endpoint the
 * chooser reads and the only thing that can prove the tile appeared or went.
 *
 * ## Why the catalogue is read over the API and not off the chooser page
 *
 * The chooser renders one tile per key of that map, so the two are the same
 * fact. The API read names the KEY; a tile assertion would name the LABEL,
 * which the catalogue's own metadata decides and which changes when the SDK
 * metadata projection lands. The chooser PAGE is still opened at the end, to
 * prove a withheld type does not break the screen that offers the rest.
 *
 * ## State is restored
 *
 * The last step reverts the type to its default, so a later journey sees the
 * catalogue this one found. `SUBJECT` is `database`, which no other journey
 * creates or attaches: disabling `github` would break J17.2 and J17.3 if the
 * projects ever run in parallel.
 */
import { test as adminTest, expect, type Page } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { checkA11y } from '../../fixtures/axe';

adminTest.use({ storageState: STORAGE_STATE.admin });

/** The type this journey decides about. See the header on why not `github`. */
const SUBJECT = 'database';

/** The project the admin persona is a member of, seeded by `e2e-stack.sh`. */
const PROJECT_ID = 1;

/** A project this persona is NOT reading below. It exists only to prove a grant is scoped. */
const OTHER_PROJECT_ID = 90200;

const POLICY_URL = `/api/v2/admin/toolkit_types/administration/${SUBJECT}`;
const CATALOGUE_URL = `/api/v2/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`;

async function openToolkitTypes(page: Page): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/toolkits', {
    waitUntil: 'domcontentloaded',
  });
  expect(response?.status(), 'the admin SPA must serve the toolkits route, not 404').toBeLessThan(
    400,
  );
  await expect(page.getByRole('heading', { name: 'Toolkits' })).toBeVisible({ timeout: 20_000 });
}

/** The key set the "+ Toolkit" chooser renders one tile from. */
async function servedTypes(page: Page): Promise<string[]> {
  const response = await page.request.get(BASE_URL + CATALOGUE_URL);
  expect(response.status(), 'the served toolkit catalogue must answer').toBe(200);
  const body = (await response.json()) as Record<string, unknown>;
  return Object.keys(body);
}

async function decide(page: Page, availability: string, reason: string): Promise<void> {
  const response = await page.request.put(BASE_URL + POLICY_URL, {
    data: { availability, reason },
  });
  expect(response.status(), `the ${availability} decision must be accepted`).toBe(200);
}

async function grant(page: Page, projectId: number, availability: string): Promise<void> {
  const response = await page.request.put(BASE_URL + `${POLICY_URL}/projects/${projectId}`, {
    data: { availability, reason: 'JRNY-038 exception' },
  });
  expect(response.status(), 'the project exception must be accepted').toBe(200);
}

adminTest('J38: the page lists every toolkit type this platform can serve', async ({ page }) => {
  await openToolkitTypes(page);

  await expect(
    page.getByRole('grid', { name: 'Toolkit types this platform can serve' }),
  ).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('admin-toolkit-types-unavailable')).toHaveCount(0);
  await expect(page.getByTestId('admin-toolkit-types-error')).toHaveCount(0);

  // ABSENCE IS NOT A DECISION. A stack that has recorded no policy must show
  // the types, not an empty grid — the failure migration 0114's header names.
  await expect(page.getByRole('row').nth(1)).toBeVisible({ timeout: 20_000 });

  await checkA11y(page);
});

/*
 * THE TWO WRITERS SHARE ONE ROW, so they take turns.
 *
 * `fullyParallel: true` (playwright.config.ts) parallelises the tests WITHIN a
 * file, not only across files. J38b and J38c both decide the availability of
 * the same type — `database` — and each asserts the served catalogue right
 * after its own write, so run side by side they read each other's decisions:
 * J38b asked for `enabled` and read a catalogue J38c had just `restricted`,
 * which is the shape of the flake webkit reported.
 *
 * Serial for THESE TWO only, in their own block: J38 above is a read and stays
 * parallel, and a file-level `serial` would let one failure hide the others
 * (`scripts/e2e-journey-shape.test.mjs`, #539).
 */
adminTest.describe('deciding the availability of one type', () => {
  adminTest.describe.configure({ mode: 'serial' });

  adminTest('J38b: a disabled type leaves the served catalogue, and comes back', async ({ page }) => {
    await openToolkitTypes(page);

    expect(await servedTypes(page), 'the subject must start in the catalogue').toContain(SUBJECT);

    await decide(page, 'disabled', 'JRNY-038 withheld the type');
    expect(await servedTypes(page), 'a disabled type must leave the chooser').not.toContain(SUBJECT);

    // The rest of the catalogue survives. A filter that emptied the map would
    // pass a "the type is gone" assertion and break every toolkit.
    expect((await servedTypes(page)).length, 'the other types must stay').toBeGreaterThan(0);

    await decide(page, 'enabled', 'JRNY-038 restored the type');
    expect(await servedTypes(page), 'an enabled type must return').toContain(SUBJECT);

    // The revert path, which is the control an operator uses to undo a mistake.
    await decide(page, 'default', '');
    expect(await servedTypes(page), 'a reverted type stays in the chooser').toContain(SUBJECT);
  });

  adminTest('J38c: a restricted type reaches only the projects granted it', async ({ page }) => {
    await openToolkitTypes(page);

    await decide(page, 'restricted', 'JRNY-038 restricted the type');
    expect(
      await servedTypes(page),
      'a restricted type with no exception must leave the chooser',
    ).not.toContain(SUBJECT);

    // A grant for ANOTHER project must not admit it here. This is the whole point
    // of a per-project exception, and it is the direction a positive-only test
    // cannot see.
    await grant(page, OTHER_PROJECT_ID, 'enabled');
    expect(
      await servedTypes(page),
      "another project's exception must not admit the type here",
    ).not.toContain(SUBJECT);

    await grant(page, PROJECT_ID, 'enabled');
    expect(await servedTypes(page), 'the granted project must get the type').toContain(SUBJECT);

    // Reverting takes the exceptions with it, which is the only correct reading
    // of "revert to default".
    await decide(page, 'default', '');
    expect(await servedTypes(page), 'a reverted type stays in the chooser').toContain(SUBJECT);

    // The chooser page still works. A withheld type must never break the screen
    // that offers the rest of them.
    const response = await page.goto(BASE_URL + '/app/toolkits/create', {
      waitUntil: 'domcontentloaded',
    });
    expect(response?.status(), 'the toolkit chooser must still render').toBeLessThan(400);
    await expect(page.getByRole('button', { name: 'GitHub', exact: true })).toBeVisible({
      timeout: 20_000,
    });
  });
});
