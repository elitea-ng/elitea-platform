/**
 * P13 rejudge — `artifacts/bucket-permissions` folder (ELITEA-2479, 2481,
 * 2482, 2483). See `e2e/journeys/artifacts/artifacts.bucket-access.spec.ts`
 * (J20h/i/j) for the core mechanics this file builds on: the exceptions
 * dialog, an authored exception reaching the server, and a stored exception
 * changing what the server allows. This file covers ground J20h/i/j do not:
 * editing/removing an EXISTING exception through the dialog's own controls,
 * and what a restricted member SEES in the UI (sidebar visibility, preview/
 * download/upload).
 *
 * `BucketAccessDialog.tsx`'s real shape differs from the onetest source's
 * guessed UI in two ways worth recording up front (each test below notes its
 * own):
 *   - There is no separate "Add exception" sub-modal with its own header/
 *     description text — one dialog holds an inline Users autocomplete, a
 *     permission Select, and an "Add exception" button (ELITEA-2478 is
 *     COVERED-EXISTING against `bucket-access.spec.ts`'s J20i on this basis).
 *   - There is no bulk-select/bulk-edit machinery at all (ELITEA-2480 is NA —
 *     no checkbox, no "Select All", no bulk Edit icon anywhere in the
 *     component).
 *   - Editing an existing row is a single inline `Select` per row (not a
 *     pencil icon opening its own modal), AND the row also carries a real
 *     Delete icon — so ELITEA-2481's "there is no separate Delete button"
 *     claim is false on this build; the REMOVE-VIA-Read/write behaviour it
 *     also describes is real, and that is what this file asserts.
 */
import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';

const BUCKET = `${AUTOTEST_PREFIX}p13-bucket-perm`;
const FILE_NAME = 'p13-bucket-perm.txt';
const FILE_BODY = 'p13 bucket permission fixture';
const MEMBER_EMAIL = 'e2e-member@autotest.local';
const ARTIFACTS_URL = `${BASE_URL}/app/artifacts`;

async function openArtifacts(page: Page): Promise<void> {
  const response = await page.goto(ARTIFACTS_URL);
  expect(response?.status(), 'the artifacts page must be served at /app/artifacts').toBeLessThan(400);
}

async function reenterArtifacts(page: Page, url: string = ARTIFACTS_URL): Promise<void> {
  await page.waitForLoadState('networkidle');
  const response = await page.goto(url);
  expect(response?.status()).toBeLessThan(400);
  await page.waitForURL('**/artifacts**', { timeout: 15_000 });
}

async function selectedProjectId(page: Page): Promise<string> {
  const id = await page.evaluate(() => localStorage.getItem('el.project.id'));
  expect(id, 'app-shell must have persisted a selected project').not.toBeNull();
  return id as string;
}

async function seedBucket(request: APIRequestContext, projectId: string): Promise<void> {
  const created = await request.post(`/api/v2/artifacts/buckets/${projectId}`, { data: { name: BUCKET } });
  expect([200, 201, 409]).toContain(created.status());
  const uploaded = await request.post(`/api/v2/artifacts/objects/${projectId}/${BUCKET}?overwrite=true`, {
    multipart: { file: { name: FILE_NAME, mimeType: 'text/plain', buffer: Buffer.from(FILE_BODY) } },
  });
  expect(uploaded.status(), await uploaded.text()).toBe(201);
}

async function memberUserId(request: APIRequestContext, projectId: string): Promise<number> {
  const response = await request.get(`/api/v2/admin/users/default/${projectId}?limit=100&offset=0`);
  expect(response.status(), await response.text()).toBe(200);
  const body = (await response.json()) as { rows: Array<{ id: string; email: string }> };
  const member = body.rows.find((row) => row.email === MEMBER_EMAIL);
  expect(member, `${MEMBER_EMAIL} must be a member of project ${projectId}`).toBeDefined();
  return Number((member as { id: string }).id);
}

async function clearExceptions(request: APIRequestContext, projectId: string, userId: number): Promise<void> {
  const response = await request.put(`/api/v2/artifacts/bucket_permissions/${projectId}`, {
    data: { user_id: userId, bucket_permissions: {} },
  });
  expect([200, 403]).toContain(response.status());
}

async function setException(
  request: APIRequestContext,
  projectId: string,
  userId: number,
  access: readonly string[],
): Promise<void> {
  const response = await request.put(`/api/v2/artifacts/bucket_permissions/${projectId}`, {
    data: { user_id: userId, bucket_permissions: { [BUCKET]: access } },
  });
  expect(response.status(), await response.text()).toBe(200);
}

test.describe('bucket permission exceptions: edit/remove/enforcement (ELITEA-2479/2481/2482/2483)', () => {
  test.describe.configure({ mode: 'serial' });

  /* onetest: ELITEA-2479 — editing an existing exception changes its stored permission. The real
   * control is an inline per-row Select (no pencil icon, no separate Edit modal): changing it fires
   * the same write J20i already proved lands, this test just proves it also lands for an EXISTING row. */
  test('ELITEA-2479: changing an existing exception\'s Select updates its stored permission', async ({
    page,
    request,
  }) => {
    await openArtifacts(page);
    const projectId = await selectedProjectId(page);
    await seedBucket(request, projectId);
    const userId = await memberUserId(request, projectId);
    await clearExceptions(request, projectId, userId);
    await setException(request, projectId, userId, ['read']);

    try {
      await reenterArtifacts(page);
      await page.getByLabel(`Manage access to ${BUCKET}`).click();
      await expect(page.getByTestId('bucket-access-dialog')).toBeVisible();

      const permissionSelect = page.getByLabel(/^Permissions for /);
      await expect(permissionSelect).toHaveText('Read-only');

      await permissionSelect.click();
      await Promise.all([
        page.waitForResponse(
          (response) =>
            response.url().includes('/api/v2/artifacts/bucket_permissions/') &&
            response.request().method() === 'PUT',
          { timeout: 15_000 },
        ),
        page.getByRole('option', { name: 'No access' }).click(),
      ]);

      await expect(permissionSelect).toHaveText('No access');

      const stored = await request.get(`/api/v2/artifacts/bucket_permissions/${projectId}`);
      expect(stored.status(), await stored.text()).toBe(200);
      const body = (await stored.json()) as { rows: Array<{ user_id: number; bucket_permissions: Record<string, string[]> }> };
      const row = body.rows.find((entry) => entry.user_id === userId);
      expect(row?.bucket_permissions[BUCKET]).toEqual([]);
    } finally {
      await clearExceptions(request, projectId, userId);
    }
  });

  /* onetest: ELITEA-2481 — selecting "Read/write (default)" on an existing exception removes it from
   * the Exceptions table (restores default access) rather than storing `["read","write"]`. The case's
   * "no separate Delete button" claim does not hold on this build (a real per-row Delete icon exists
   * too) — this test asserts only the remove-via-default-access behaviour, which is real. */
  test('ELITEA-2481: setting an existing exception to Read/write removes it from Exceptions', async ({
    page,
    request,
  }) => {
    await openArtifacts(page);
    const projectId = await selectedProjectId(page);
    await seedBucket(request, projectId);
    const userId = await memberUserId(request, projectId);
    await clearExceptions(request, projectId, userId);
    await setException(request, projectId, userId, ['read']);

    try {
      await reenterArtifacts(page);
      await page.getByLabel(`Manage access to ${BUCKET}`).click();
      await expect(page.getByTestId('bucket-access-dialog')).toBeVisible();

      const countBefore = await page.getByText(/^Exceptions – \d+$/).textContent();
      expect(countBefore).toContain('1');

      const permissionSelect = page.getByLabel(/^Permissions for /);
      await permissionSelect.click();
      await Promise.all([
        page.waitForResponse(
          (response) =>
            response.url().includes('/api/v2/artifacts/bucket_permissions/') &&
            response.request().method() === 'PUT',
          { timeout: 15_000 },
        ),
        page.getByRole('option', { name: 'Read/write (default)' }).click(),
      ]);

      await expect(page.getByLabel(/^Permissions for /)).toHaveCount(0);
      await expect(page.getByText('Exceptions – 0')).toBeVisible();

      const stored = await request.get(`/api/v2/artifacts/bucket_permissions/${projectId}`);
      expect(stored.status(), await stored.text()).toBe(200);
      const body = (await stored.json()) as { rows: Array<{ user_id: number; bucket_permissions: Record<string, string[]> }> };
      const row = body.rows.find((entry) => entry.user_id === userId);
      expect(row === undefined || !Object.prototype.hasOwnProperty.call(row.bucket_permissions, BUCKET)).toBe(true);
    } finally {
      await clearExceptions(request, projectId, userId);
    }
  });

  /* onetest: ELITEA-2482 — a "No access" exception hides the bucket from the restricted member's
   * sidebar, and a direct URL to it is refused server-side too (the UI has no per-bucket path segment
   * — `?bucket=` is a query param — so "direct URL" here is that query param). */
  test('ELITEA-2482: a "No access" exception hides the bucket and refuses direct access', async ({
    browser,
    request,
  }) => {
    const adminCtx = await browser.newContext({ storageState: STORAGE_STATE.admin });
    const adminPage = await adminCtx.newPage();
    await openArtifacts(adminPage);
    const projectId = await selectedProjectId(adminPage);
    await seedBucket(request, projectId);
    const userId = await memberUserId(request, projectId);
    await clearExceptions(request, projectId, userId);
    await adminCtx.close();

    await setException(request, projectId, userId, []);

    try {
      const memberCtx = await browser.newContext({ storageState: STORAGE_STATE.member });
      const memberPage = await memberCtx.newPage();
      await openArtifacts(memberPage);
      await expect(memberPage.getByLabel(`Manage access to ${BUCKET}`)).toHaveCount(0, { timeout: 15_000 });

      const direct = await memberPage.goto(`${ARTIFACTS_URL}?bucket=${BUCKET}`);
      expect(direct?.status(), 'the shell route itself is still served').toBeLessThan(400);
      await memberPage.waitForURL('**/artifacts**', { timeout: 15_000 });
      // No access: the file table for THIS bucket never renders — the row this
      // bucket's own seed created must stay invisible to the restricted member.
      await expect(memberPage.getByRole('row').filter({ hasText: FILE_NAME })).toHaveCount(0, { timeout: 10_000 });

      const blockedRead = await memberCtx.request.get(`/api/v2/artifacts/objects/${projectId}/${BUCKET}`);
      expect(blockedRead.status()).toBe(403);

      await memberCtx.close();
    } finally {
      await clearExceptions(request, projectId, userId);
    }
  });

  /* onetest: ELITEA-2483 — a "Read-only" exception still lets the restricted member preview/download,
   * but an upload attempt is refused with an error shown on the page. The onetest source guesses the
   * exact wording ("You have read-only permission for this bucket"); the real message
   * (`useArtifactUpload.ts`) is generic — `Failed to upload: <name>.` — this test asserts the real one. */
  test('ELITEA-2483: a "Read-only" exception allows preview/download but blocks upload', async ({
    browser,
    request,
  }) => {
    const adminCtx = await browser.newContext({ storageState: STORAGE_STATE.admin });
    const adminPage = await adminCtx.newPage();
    await openArtifacts(adminPage);
    const projectId = await selectedProjectId(adminPage);
    await seedBucket(request, projectId);
    const userId = await memberUserId(request, projectId);
    await clearExceptions(request, projectId, userId);
    await adminCtx.close();

    await setException(request, projectId, userId, ['read']);

    try {
      const memberCtx = await browser.newContext({ storageState: STORAGE_STATE.member });
      const memberPage = await memberCtx.newPage();
      await openArtifacts(memberPage);
      await memberPage.goto(`${ARTIFACTS_URL}?bucket=${BUCKET}`);
      await memberPage.waitForURL('**/artifacts**', { timeout: 15_000 });

      const row = memberPage.getByRole('row').filter({ hasText: FILE_NAME });
      await expect(row).toBeVisible({ timeout: 15_000 });

      await memberPage.getByRole('button', { name: `Preview ${FILE_NAME}`, exact: true }).click();
      await expect(memberPage.getByText(FILE_BODY)).toBeVisible({ timeout: 15_000 });

      const [download] = await Promise.all([
        memberPage.waitForEvent('download', { timeout: 15_000 }),
        memberPage.getByRole('button', { name: `Download ${FILE_NAME}`, exact: true }).click(),
      ]);
      expect(download.suggestedFilename()).toBe(FILE_NAME);

      const uploadName = 'p13-readonly-upload-attempt.bin';
      const [chooser] = await Promise.all([
        memberPage.waitForEvent('filechooser', { timeout: 15_000 }),
        memberPage.getByRole('button', { name: 'Upload files' }).click(),
      ]);
      await chooser.setFiles({ name: uploadName, mimeType: 'application/octet-stream', buffer: Buffer.alloc(16, 1) });
      await memberPage.getByRole('button', { name: 'Continue', exact: true }).click();

      await expect(memberPage.getByRole('alert')).toContainText(`Failed to upload: ${uploadName}`, {
        timeout: 15_000,
      });
      await expect(memberPage.getByRole('row').filter({ hasText: uploadName })).toHaveCount(0);

      await memberCtx.close();
    } finally {
      await clearExceptions(request, projectId, userId);
    }
  });
});
