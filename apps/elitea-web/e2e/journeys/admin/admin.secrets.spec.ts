/**
 * Journey 32: The admin Secrets page edits the GLOBAL vault — the right store —
 *             and its writes survive a reload (JRNY-032)
 *
 * ## Why this journey needs to exist at all
 *
 * Before unit A14 the `administration` mode of the secrets routes answered 501,
 * because every method on the project handler is keyed by the `{projectID}`
 * segment and admin_ui sends the placeholder `0` there — so serving this page
 * from it would have read and WRITTEN `project-0`'s vault. A journey that
 * asserted only "a table of secret names is present" would pass against exactly
 * that mistake, because a wrong-store read looks identical to a right-store one.
 *
 * So the assertions here are all against things only the correct store can
 * produce:
 *
 *  - The five `chat_max_*` entries are seeded by `scripts/e2e-stack.sh seed`
 *    into the `admin` vault ROW and into no other. They are Fernet blobs written
 *    as SQL literals, so they exist in no bundle and no fixture the client can
 *    reach; seeing them proves the page decrypted the global vault. Project 1's
 *    vault is seeded EMPTY, so a handler that fell back to a project row would
 *    render nothing at all.
 *  - The write is verified by a RELOAD and a re-read, not by the toast. A
 *    handler that answered 200 and wrote nothing is the defect #130/#180 shipped
 *    twice.
 *  - After the write, Settings › Secrets (the PROJECT vault, project 1) is
 *    checked and must still be empty. That is the store-separation assertion:
 *    the one failure mode the 501 existed to prevent, caught from the other end.
 *
 * The journey creates its own probe and deletes it again, so it is re-runnable
 * against a stack that was not re-seeded — and it never touches the five seeded
 * entries, which J20f depends on.
 *
 * No value used here is or resembles a credential.
 */
import { test as adminTest, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

/** Seeded into the `admin` vault row by `scripts/e2e-stack.sh seed`. */
const SEEDED_GLOBAL_SECRET = 'chat_max_upload_count';

/**
 * This journey's own row. Created, edited and deleted within the run.
 *
 * The name is per-BROWSER-PROJECT because the E2E stack is ONE database and
 * chromium and webkit run this spec concurrently. A shared name made them race:
 * whichever finished first deleted the row the other was mid-way through
 * editing, and the failure looked like "the edit button never appeared" rather
 * than like the cross-talk it was.
 */
const PROBE_VALUE = 'marker-one';
const PROBE_UPDATED = 'marker-two';

function probeName(projectName: string): string {
  return `e2e_secrets_probe_${projectName.replace(/[^A-Za-z0-9_]/g, '_')}`;
}

async function openAdminSecrets(page: Page): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/secrets', { waitUntil: 'domcontentloaded' });
  expect(response?.status(), 'the admin SPA must serve the secrets route, not 404').toBeLessThan(400);
  await expect(page.getByRole('grid')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('admin-secrets-unavailable')).toHaveCount(0);
}

/** Removes the probe if a previous run left it behind, so the run is repeatable. */
async function deleteProbeIfPresent(page: Page, probe: string): Promise<void> {
  const remove = page.getByRole('button', { name: `Delete: ${probe}` });
  if ((await remove.count()) === 0) return;
  await remove.click();
  const modal = page.getByTestId('admin-secrets-delete-modal');
  await modal.getByRole('textbox').fill(probe);
  await Promise.all([
    page.waitForResponse(
      (r) => r.url().includes(`/secrets/secret/administration/0/${probe}`) && r.request().method() === 'DELETE',
    ),
    modal.getByRole('button', { name: /confirm|delete/i }).click(),
  ]);
  await expect(page.getByText(probe, { exact: true })).toHaveCount(0);
}

adminTest('J32: the page lists the global vault, which is not any project vault', async ({ page }) => {
  await openAdminSecrets(page);

  // From the `admin` ROW of centry.secrets_data, decrypted server-side. This
  // string is in no bundle and in no fixture the browser can reach.
  await expect(page.getByText(SEEDED_GLOBAL_SECRET, { exact: true })).toBeVisible();

  // The listing is masked. The plaintext must not be on screen until asked for.
  await expect(page.getByText('4', { exact: true })).toHaveCount(0);

  // Reveal it — the one place a value is fetched — and check it is the SEEDED
  // value, not a default. `chat_max_upload_count` is seeded to 4; the
  // application's built-in default is 10, so a page reading anything other than
  // this vault would show the wrong number here.
  const row = page.getByRole('row').filter({ hasText: SEEDED_GLOBAL_SECRET });
  const [revealed] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes(`/secrets/secret/administration/0/${SEEDED_GLOBAL_SECRET}`) &&
        r.request().method() === 'GET',
    ),
    row.getByRole('button', { name: /show|reveal/i }).click(),
  ]);
  expect(revealed.status(), 'the reveal must be authorised server-side').toBe(200);
  await expect(row.getByText('4', { exact: true })).toBeVisible();

  await checkA11y(page);
});

adminTest('J32b: creating a global secret survives a reload, and does not touch the project vault', async ({
  page,
}, testInfo) => {
  // Four page loads, three writes and three reloads in one journey: it takes
  // about 25 s on a green webkit run, so the default 30 s left no headroom and
  // a slower runner timed it out on its last step (the delete), with no step
  // ever hanging.
  adminTest.setTimeout(60_000);
  const PROBE_NAME = probeName(testInfo.project.name);

  await openAdminSecrets(page);
  await deleteProbeIfPresent(page, PROBE_NAME);

  // ── create ───────────────────────────────────────────────────────────────
  await page.getByRole('button', { name: 'Create secret' }).click();
  const dialog = page.getByTestId('admin-secret-dialog');
  await dialog.getByLabel(/Secret name/).fill(PROBE_NAME);
  await dialog.getByLabel(/Secret value/).fill(PROBE_VALUE);
  const [created] = await Promise.all([
    page.waitForResponse(
      (r) => r.url().includes(`/secrets/secret/administration/0/${PROBE_NAME}`) && r.request().method() === 'POST',
    ),
    dialog.getByRole('button', { name: 'Create' }).click(),
  ]);
  // 200 proves the write was AUTHORISED. It does not prove it was written —
  // that is what the reload below is for.
  expect(created.status(), 'the create must be authorised server-side').toBe(200);
  await expect(page.getByText('Secret created.')).toBeVisible();

  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('grid')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByText(PROBE_NAME, { exact: true })).toBeVisible();

  // ── the value is the one that was written ────────────────────────────────
  let row = page.getByRole('row').filter({ hasText: PROBE_NAME });
  await row.getByRole('button', { name: /show|reveal/i }).click();
  await expect(row.getByText(PROBE_VALUE, { exact: true })).toBeVisible();

  // ── the PROJECT vault did not move ───────────────────────────────────────
  // Settings › Secrets reads `project-1`, which the seed leaves EMPTY. If the
  // administration write had been keyed on the placeholder project id, or if
  // the page had fallen back to `default` mode, the probe would be here.
  await page.goto(BASE_URL + '/app/settings/secrets', { waitUntil: 'domcontentloaded' });
  // Wait for the project listing to have SETTLED before asserting an absence.
  // `Create new secret` carries `disabled: isFetching`, so it becoming enabled
  // is the page's own signal that the query finished — without it, "the probe is
  // not here" would also be true of a page that had not loaded yet.
  //
  // It doubles as the fix for a webkit-only flake: this route rewrites its own
  // search params right after mount, and navigating away before that lands
  // aborts the next `goto` with "interrupted by another navigation".
  await expect(page.getByRole('button', { name: 'Create new secret', exact: true })).toBeEnabled({
    timeout: 15_000,
  });
  await expect(page.getByText(PROBE_NAME, { exact: true })).toHaveCount(0);
  await expect(page.getByText(SEEDED_GLOBAL_SECRET, { exact: true })).toHaveCount(0);

  // ── edit, and re-read after a reload ─────────────────────────────────────
  await openAdminSecrets(page);
  await page.getByRole('button', { name: `Edit: ${PROBE_NAME}` }).click();
  const editDialog = page.getByTestId('admin-secret-dialog');
  await editDialog.getByLabel(/New value/).fill(PROBE_UPDATED);
  const [updated] = await Promise.all([
    page.waitForResponse(
      (r) => r.url().includes(`/secrets/secret/administration/0/${PROBE_NAME}`) && r.request().method() === 'PUT',
    ),
    editDialog.getByRole('button', { name: 'Save' }).click(),
  ]);
  expect(updated.status(), 'the update must be authorised server-side').toBe(200);

  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('grid')).toBeVisible({ timeout: 20_000 });
  row = page.getByRole('row').filter({ hasText: PROBE_NAME });
  await row.getByRole('button', { name: /show|reveal/i }).click();
  await expect(row.getByText(PROBE_UPDATED, { exact: true })).toBeVisible();

  // ── delete, and confirm it is gone after a reload ────────────────────────
  await deleteProbeIfPresent(page, PROBE_NAME);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('grid')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByText(PROBE_NAME, { exact: true })).toHaveCount(0);

  // …and the five seeded entries are still there. Every write above edited the
  // vault rather than replacing it; replacing it would have deleted these, and
  // J20f depends on them.
  await expect(page.getByText(SEEDED_GLOBAL_SECRET, { exact: true })).toBeVisible();
});
