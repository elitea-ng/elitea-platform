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
 *
 * ## Every test owns its own bucket AND writes exceptions cooperatively —
 * neither is optional under `fullyParallel`
 *
 * `PUT /artifacts/bucket_permissions/{projectId}` is NOT a per-bucket
 * upsert: the repo (`artifact_bucket_permissions.go`) `DELETE`s every OTHER
 * bucket this user has an exception for that is NOT named in the request,
 * then upserts each bucket the request DOES name — i.e. it REPLACES the
 * caller's entire exception set for that user. Only two personas can log in
 * at all (`e2e-member`/`e2e-admin`), so every test in this file necessarily
 * sets an exception for the SAME user row — a naive "read nothing, write
 * just my bucket" call would silently DELETE whatever exception a
 * concurrently-running sibling test just wrote for ITS OWN bucket, on the
 * SAME user. `setExceptionMerged`/`clearExceptionMerged` below always
 * read-merge-write the FULL current map (never blind-overwrite) and confirm
 * their own bucket landed before returning, retrying the whole cycle if a
 * concurrent writer's own read-merge-write raced ahead of this one — safe
 * under `fullyParallel` with no `serial` mode and no shared bucket.
 */
import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

const FILE_BODY = 'p13 bucket permission fixture';
const MEMBER_EMAIL = 'e2e-member@autotest.local';
const ARTIFACTS_URL = `${BASE_URL}/app/artifacts`;

/** Run-unique AND test-unique: two tests (or two `--repeat-each` copies of the same test) must never share a bucket. */
function uniqueBucket(tag: string): string {
  return `autotest-p13-bp-${tag}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
}

/**
 * A synthetic exception TARGET — not a real login. `PUT bucket_permissions` does not validate that
 * `user_id` names a real project member (measured directly: it accepts and stores any integer), and
 * `BucketAccessDialog.tsx` renders a nameless row as `#<userId>` — everything ELITEA-2479/2481 need
 * (an EXISTING exception row whose Select they edit) works identically. Using one of these per test,
 * instead of the one real `e2e-member` id, means these two tests' own UI-driven writes (the dialog's
 * `onSetAccess` mutation, which read-merge-writes the FULL map from the CLIENT's own cached read, not
 * from a fresh server read) never collide with each other OR with ELITEA-2482/2483's real `e2e-member`
 * row — those two never trigger that mutation at all (no permission-editing UI action), so their own
 * setup/cleanup writes go through `setExceptionMerged`'s safe read-merge-write-confirm cycle alone.
 */
function syntheticUserId(): number {
  return 900_000_000 + Math.floor(Math.random() * 99_999_999);
}

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

async function seedBucket(request: APIRequestContext, projectId: string, bucket: string, fileName: string): Promise<void> {
  const created = await request.post(`/api/v2/artifacts/buckets/${projectId}`, { data: { name: bucket } });
  expect([200, 201, 409]).toContain(created.status());
  const uploaded = await request.post(`/api/v2/artifacts/objects/${projectId}/${bucket}?overwrite=true`, {
    multipart: { file: { name: fileName, mimeType: 'text/plain', buffer: Buffer.from(FILE_BODY) } },
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

/** This user's full current exception map — the read half of every read-merge-write below. */
async function readOwnMap(
  request: APIRequestContext,
  projectId: string,
  userId: number,
): Promise<Record<string, readonly string[]>> {
  const response = await request.get(`/api/v2/artifacts/bucket_permissions/${projectId}`);
  expect(response.status(), await response.text()).toBe(200);
  const body = (await response.json()) as {
    rows: Array<{ user_id: number; bucket_permissions: Record<string, readonly string[]> }>;
  };
  const found = body.rows.find((row) => row.user_id === userId)?.bucket_permissions;
  return found === undefined ? {} : { ...found };
}

/**
 * Sets (or removes, when `access` is `undefined`) exactly ONE bucket's exception for this user,
 * read-merge-writing the user's FULL map so a concurrent sibling test's own exception (a different
 * bucket, same user row) is never blown away — and retries the whole read→merge→write→confirm cycle
 * if a concurrent writer raced ahead between this call's read and write. See file header.
 */
async function setExceptionMerged(
  request: APIRequestContext,
  projectId: string,
  userId: number,
  bucket: string,
  access: readonly string[] | undefined,
): Promise<void> {
  for (let attempt = 0; attempt < 8; attempt += 1) {
    const current = await readOwnMap(request, projectId, userId);
    const merged = { ...current };
    if (access === undefined) delete merged[bucket];
    else merged[bucket] = access;

    const put = await request.put(`/api/v2/artifacts/bucket_permissions/${projectId}`, {
      data: { user_id: userId, bucket_permissions: merged },
    });
    expect(put.status(), await put.text()).toBe(200);

    const after = await readOwnMap(request, projectId, userId);
    const landed = access === undefined
      ? !Object.prototype.hasOwnProperty.call(after, bucket)
      : Array.isArray(after[bucket]) && [...(after[bucket] ?? [])].sort().join(',') === [...access].sort().join(',');
    if (landed) return;
    await new Promise((resolve) => setTimeout(resolve, 100 + attempt * 100));
  }
  throw new Error(`setExceptionMerged: exception for bucket "${bucket}" never confirmed after retries`);
}

test.describe('bucket permission exceptions: edit/remove/enforcement (ELITEA-2479/2481/2482/2483)', () => {
  /* onetest: ELITEA-2479 — editing an existing exception changes its stored permission. The real
   * control is an inline per-row Select (no pencil icon, no separate Edit modal): changing it fires
   * the same write J20i already proved lands, this test just proves it also lands for an EXISTING row. */
  test('ELITEA-2479: changing an existing exception\'s Select updates its stored permission', async ({
    page,
    request,
  }) => {
    const bucket = uniqueBucket('2479');
    const fileName = `${bucket}.txt`;
    const userId = syntheticUserId();
    await openArtifacts(page);
    const projectId = await selectedProjectId(page);
    await seedBucket(request, projectId, bucket, fileName);
    await setExceptionMerged(request, projectId, userId, bucket, ['read']);

    try {
      await reenterArtifacts(page);
      await page.getByLabel(`Manage access to ${bucket}`).click();
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

      await expect
        .poll(async () => (await readOwnMap(request, projectId, userId))[bucket], { timeout: 10_000 })
        .toEqual([]);
    } finally {
      await setExceptionMerged(request, projectId, userId, bucket, undefined);
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
    const bucket = uniqueBucket('2481');
    const fileName = `${bucket}.txt`;
    const userId = syntheticUserId();
    await openArtifacts(page);
    const projectId = await selectedProjectId(page);
    await seedBucket(request, projectId, bucket, fileName);
    await setExceptionMerged(request, projectId, userId, bucket, ['read']);

    try {
      await reenterArtifacts(page);
      await page.getByLabel(`Manage access to ${bucket}`).click();
      await expect(page.getByTestId('bucket-access-dialog')).toBeVisible();

      // Auto-retrying, not a one-shot read: the dialog's own list query can still be in flight the
      // instant it becomes visible, and a one-shot `textContent()` here caught that transient "– 0".
      await expect(page.getByText(/^Exceptions – \d+$/)).toHaveText('Exceptions – 1', { timeout: 10_000 });

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

      await expect
        .poll(
          async () => Object.prototype.hasOwnProperty.call(await readOwnMap(request, projectId, userId), bucket),
          { timeout: 10_000 },
        )
        .toBe(false);
    } finally {
      await setExceptionMerged(request, projectId, userId, bucket, undefined);
    }
  });

  /* onetest: ELITEA-2482 — a "No access" exception hides the bucket from the restricted member's
   * sidebar, and a direct URL to it is refused server-side too (the UI has no per-bucket path segment
   * — `?bucket=` is a query param — so "direct URL" here is that query param). */
  test('ELITEA-2482: a "No access" exception hides the bucket and refuses direct access', async ({
    browser,
    request,
  }) => {
    const bucket = uniqueBucket('2482');
    const fileName = `${bucket}.txt`;
    const adminCtx = await browser.newContext({ storageState: STORAGE_STATE.admin });
    const adminPage = await adminCtx.newPage();
    await openArtifacts(adminPage);
    const projectId = await selectedProjectId(adminPage);
    await seedBucket(request, projectId, bucket, fileName);
    const userId = await memberUserId(request, projectId);
    await adminCtx.close();

    await setExceptionMerged(request, projectId, userId, bucket, []);

    try {
      const memberCtx = await browser.newContext({ storageState: STORAGE_STATE.member });
      const memberPage = await memberCtx.newPage();
      await openArtifacts(memberPage);
      await expect(memberPage.getByLabel(`Manage access to ${bucket}`)).toHaveCount(0, { timeout: 15_000 });

      const direct = await memberPage.goto(`${ARTIFACTS_URL}?bucket=${bucket}`);
      expect(direct?.status(), 'the shell route itself is still served').toBeLessThan(400);
      await memberPage.waitForURL('**/artifacts**', { timeout: 15_000 });
      // No access: the file table for THIS bucket never renders — the row this
      // bucket's own seed created must stay invisible to the restricted member.
      await expect(memberPage.getByRole('row').filter({ hasText: fileName })).toHaveCount(0, { timeout: 10_000 });

      const blockedRead = await memberCtx.request.get(`/api/v2/artifacts/objects/${projectId}/${bucket}`);
      expect(blockedRead.status()).toBe(403);

      await memberCtx.close();
    } finally {
      await setExceptionMerged(request, projectId, userId, bucket, undefined);
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
    const bucket = uniqueBucket('2483');
    const fileName = `${bucket}.txt`;
    const adminCtx = await browser.newContext({ storageState: STORAGE_STATE.admin });
    const adminPage = await adminCtx.newPage();
    await openArtifacts(adminPage);
    const projectId = await selectedProjectId(adminPage);
    await seedBucket(request, projectId, bucket, fileName);
    const userId = await memberUserId(request, projectId);
    await adminCtx.close();

    await setExceptionMerged(request, projectId, userId, bucket, ['read']);

    try {
      const memberCtx = await browser.newContext({ storageState: STORAGE_STATE.member });
      const memberPage = await memberCtx.newPage();
      await openArtifacts(memberPage);
      await memberPage.goto(`${ARTIFACTS_URL}?bucket=${bucket}`);
      await memberPage.waitForURL('**/artifacts**', { timeout: 15_000 });

      const row = memberPage.getByRole('row').filter({ hasText: fileName });
      await expect(row).toBeVisible({ timeout: 15_000 });

      await memberPage.getByRole('button', { name: `Preview ${fileName}`, exact: true }).click();
      await expect(memberPage.getByText(FILE_BODY)).toBeVisible({ timeout: 15_000 });

      // Re-enter (a fresh load, not history back) before Download: the preview
      // pane replaces the file table, exactly as artifacts.lifecycle.spec.ts's
      // J20d documents for this same sequence.
      await memberPage.waitForLoadState('networkidle');
      await memberPage.goto(`${ARTIFACTS_URL}?bucket=${bucket}`);
      await memberPage.waitForURL('**/artifacts**', { timeout: 15_000 });
      await expect(row).toBeVisible({ timeout: 15_000 });

      const [download] = await Promise.all([
        memberPage.waitForEvent('download', { timeout: 15_000 }),
        memberPage.getByRole('button', { name: `Download ${fileName}`, exact: true }).click(),
      ]);
      expect(download.suggestedFilename()).toBe(fileName);

      const uploadName = `${bucket}-upload-attempt.bin`;
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
      await setExceptionMerged(request, projectId, userId, bucket, undefined);
    }
  });
});
