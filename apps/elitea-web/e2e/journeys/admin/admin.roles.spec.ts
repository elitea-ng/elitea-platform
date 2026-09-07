/**
 * Journey 30: The admin Roles matrix is the deployment's own, and saving it
 *             actually changes what the server grants (JRNY-030)
 *
 * ## Why this journey needs to exist at all
 *
 * Before unit A14 this page's PUT and POST had no route, and its GET ignored
 * the `{scope}` segment so the Public and Support tabs rendered the CENTRAL
 * administration matrix. A journey that asserted "a matrix of checkboxes is
 * present" would have passed against every one of those.
 *
 * So the assertions here are all against things only a working server can
 * produce:
 *
 *  - `e2e.roles.probe` is seeded into `auth_core__role_permission` by
 *    `scripts/e2e-stack.sh seed` and exists in no bundle. Seeing it proves the
 *    catalogue is read from the database — and it is granted to NOBODY on the
 *    `editor` role, which the pre-A14 read could not have shown at all, because
 *    it only listed permissions somebody already held.
 *  - The save is verified by a RELOAD and a re-read, not by the toast. A
 *    handler that answered 200 and wrote nothing is the exact defect
 *    #130/#180 shipped twice.
 *  - The Admin and Standard tabs are asserted to DISAGREE. They are two
 *    different target modes behind the same handler; a read that ignored the
 *    path would make them identical.
 *
 * The journey restores what it changed, so it is re-runnable against a stack
 * that was not re-seeded.
 */
import { test as adminTest, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

/** Seeded by `scripts/e2e-stack.sh seed` — see its "administration-mode RBAC" block. */
const SEEDED_PROBE = 'e2e.roles.probe';
const PROBE_GROUP = 'e2e.roles';

async function openRoles(page: Page): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/roles', { waitUntil: 'domcontentloaded' });
  expect(response?.status(), 'the admin SPA must serve the roles route, not 404').toBeLessThan(400);
  await expect(page.getByRole('table', { name: 'Permission matrix' })).toBeVisible({ timeout: 20_000 });
}

/**
 * Read the matrix from the SERVER, then reload the page and expand the probe
 * group, so the caller can assert both halves of a re-read.
 *
 * J30b's two re-reads used to be `toBeChecked()` on a checkbox after a bare
 * reload, and that is this journey's entry in #545: a checkbox is the end of a
 * chain — the server answers, the client unwraps the envelope, the matrix
 * renders, the group is expanded — so its failure could not say whether the
 * grant was missing from the server's answer or merely late on screen. Those
 * are the two candidate causes #545 requires this journey to tell apart, and
 * one assertion cannot.
 *
 * A DIRECT read, not a `waitForResponse` around the reload. The save
 * invalidates the matrix query, so the current document issues its own refetch
 * and such a wait matches THAT response — which the reload then discards,
 * leaving `response.json()` to fail with "Response body is not available for a
 * response that was navigated away from" (measured on J28, the sibling journey
 * that was corrected the same way). The direct read answers the same question
 * with no race, and the checkbox assertion after the reload is unchanged.
 */
async function reloadMatrix(page: Page): Promise<Record<string, unknown>[]> {
  const response = await page.request.get(
    `${BASE_URL}/api/v2/admin/permissions/administration/administration`,
  );
  expect(response.status(), await response.text()).toBe(200);
  const rows = ((await response.json()) as { rows: Record<string, unknown>[] }).rows;

  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('table', { name: 'Permission matrix' })).toBeVisible({ timeout: 20_000 });
  await expandProbeGroup(page);
  return rows;
}

/** The `editor` cell of the seeded probe row, as the SERVER reported it. */
function editorHoldsProbe(rows: Record<string, unknown>[]): unknown {
  const row = rows.find((r) => r['name'] === SEEDED_PROBE);
  expect(row, `the matrix must list ${SEEDED_PROBE}`).toBeTruthy();
  return row?.['editor'];
}

async function expandProbeGroup(page: Page): Promise<void> {
  const expander = page.getByRole('button', { name: `Expand permission group: ${PROBE_GROUP}` });
  await expect(expander).toBeVisible({ timeout: 20_000 });
  await expander.click();
}

adminTest('J30: the matrix lists database-only permissions and separates its tabs', async ({ page }) => {
  await openRoles(page);

  // From the DATABASE. The read is gated on
  // `configuration.roles.permissions.view`; the admin persona holds it via the
  // seed, so a 403 here would mean the gate and the seed have drifted apart.
  await expect(page.getByText(PROBE_GROUP, { exact: true })).toBeVisible();
  await expect(page.getByText('Failed to load the permission matrix.')).toHaveCount(0);

  await expandProbeGroup(page);
  // The `viewer` column holds it, `editor` does not — the pre-A14 read could
  // report neither, because a permission had to be granted to appear at all.
  await expect(page.getByRole('checkbox', { name: `viewer: ${SEEDED_PROBE}` })).toBeChecked();
  await expect(page.getByRole('checkbox', { name: `editor: ${SEEDED_PROBE}` })).not.toBeChecked();

  // The two administration tabs are different TARGET MODES behind one handler.
  // `super_admin` is an administration-mode role and is not defined for
  // `default`, so a read that ignored its path would show it in both.
  await expect(page.getByRole('columnheader', { name: 'super admin' })).toBeVisible();
  const [standardListing] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/admin/permissions/administration/default') && r.request().method() === 'GET',
    ),
    page.getByRole('tab', { name: 'Standard Roles' }).click(),
  ]);
  expect(standardListing.status(), 'the standard matrix must be authorised server-side').toBe(200);
  await expect(page.getByRole('columnheader', { name: 'super admin' })).toHaveCount(0);
  await expect(page.getByRole('columnheader', { name: 'editor' })).toBeVisible();

  await checkA11y(page);
});

adminTest('J30b: granting a permission survives a reload, and revoking it takes it away', async ({ page }) => {
  await openRoles(page);
  await expandProbeGroup(page);

  const editorCell = () => page.getByRole('checkbox', { name: `editor: ${SEEDED_PROBE}` });
  await expect(editorCell()).not.toBeChecked();

  // ── grant ────────────────────────────────────────────────────────────────
  await editorCell().click();
  const [saved] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/admin/permissions/administration/administration') &&
        r.request().method() === 'PUT',
    ),
    page.getByRole('button', { name: 'Save' }).click(),
  ]);
  // 200 proves the write was AUTHORISED. It does not prove it was written —
  // that is what the reload below is for.
  expect(saved.status(), 'the save must be authorised server-side').toBe(200);
  await expect(page.getByText('Permissions saved.')).toBeVisible();

  expect(
    editorHoldsProbe(await reloadMatrix(page)),
    'the grant must survive a full reload, in the server\'s own answer',
  ).toBe(true);
  await expect(editorCell(), 'the grant must survive a full reload').toBeChecked();

  // ── revoke, restoring the seeded state ───────────────────────────────────
  await editorCell().click();
  const [reverted] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/admin/permissions/administration/administration') &&
        r.request().method() === 'PUT',
    ),
    page.getByRole('button', { name: 'Save' }).click(),
  ]);
  expect(reverted.status()).toBe(200);

  // A save that only ever ADDS would leave this checked. Revocation is the half
  // of the diff an operator relies on and the half nothing on screen confirms.
  expect(
    editorHoldsProbe(await reloadMatrix(page)),
    'the revoke must survive a full reload, in the server\'s own answer',
  ).not.toBe(true);
  await expect(editorCell(), 'the revoke must survive a full reload').not.toBeChecked();
  // The seeded grant on `viewer` is untouched — the write is keyed on the role.
  await expect(page.getByRole('checkbox', { name: `viewer: ${SEEDED_PROBE}` })).toBeChecked();
});

/*
 * NOT COVERED here — deliberately:
 *
 *  - The Public and Support tabs. Support is legitimately unavailable on a
 *    stack with no `SUPPORT_PROJECT_ID`, and asserting the reason string here
 *    would pin an environment rather than a behaviour; the scope separation
 *    (including the inherited-matrix fallback and the "wrong scope" defect) is
 *    covered by the Go integration tests, which can seed several projects.
 *  - "Apply to Projects". It rewrites every shared project's permissions —
 *    running it inside a journey would leave the rest of the e2e suite on a
 *    matrix it did not choose. Covered by
 *    `TestSyncPushesTheCentralMatrixOntoSharedProjectsOnly`.
 *  - The refusal of a caller without `configuration.roles.permissions.edit`.
 *    Covered by `TestPermissionMatrixRefusesACallerWithoutTheEditPermission`,
 *    which can hold the view permission and not the edit one; the e2e personas
 *    hold either both or neither.
 *  - The read-only `system` column. The e2e stack seeds no administration-mode
 *    `system` role, so there is no such column to assert on, and seeding one
 *    purely to be asserted would make the journey test its own fixture. The
 *    disabled control is covered by `Roles.test.tsx` and the SERVER's refusal —
 *    the part that matters — by
 *    `TestPermissionMatrixSaveNeverWritesTheSystemRole`.
 */
