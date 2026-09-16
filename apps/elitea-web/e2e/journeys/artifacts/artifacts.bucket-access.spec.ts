/**
 * Journey 20h-20j: per-bucket ACCESS LISTS — author an exception, see it, and
 * feel it.
 *
 * The feature is the port of legacy/plugins/artifacts' `bucket_permissions`
 * surface. Its model is EXCEPTIONS to a default of full access, so the three
 * things worth proving end to end are exactly the three below:
 *
 *   J20h  the dialog opens, and it says what the default IS. An empty table is
 *         the correct answer for an unrestricted bucket, and it is
 *         indistinguishable from a broken read unless the screen states the
 *         default out loud.
 *   J20i  an exception authored in the dialog reaches the server, and the
 *         server hands it back.
 *   J20j  the exception CHANGES WHAT THE SERVER DOES. A dialog that stores a
 *         restriction nothing enforces is the failure this journey exists for:
 *         every screen looks right and the bucket is still open.
 *
 * ## Why the member persona blocks ITSELF in J20j
 *
 * The admin bypass is real (`ArtifactBucketPermissionsRepository.IsProjectAdmin`),
 * and `e2e-admin@autotest.local` holds the project `admin` role, so an
 * exception written against the admin persona proves nothing about denial.
 * `e2e-member@autotest.local` is a project EDITOR, so it is the only persona in
 * this stack an exception can actually refuse. The spec restores the state it
 * changed in the same test, and the blast radius of a mid-test failure is ONE
 * bucket that no other spec touches.
 */
import { test, expect, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

/** A bucket no other spec uses — see the header on blast radius. */
const ACL_BUCKET = 'autotest-j20-acl-art';
const ACL_FILE = 'j20-acl.txt';
const ACL_BODY = 'access list journey';

const MEMBER_EMAIL = 'e2e-member@autotest.local';

/**
 * The shell mounts at `/app` (`app/router.tsx`'s `basepath`), so the page is
 * `/app/artifacts` and never `/artifacts`.
 *
 * MEASURED, because a bare `/artifacts` fails in the one way that is easy to
 * misread: the server answers a plain-text `404 page not found` and the SPA
 * never boots at all, yet the URL still ENDS IN `/artifacts`, so
 * `waitForURL('**\/artifacts**')` is satisfied, and `selectedProjectId` still
 * returns an id because `localStorage` belongs to the ORIGIN and arrives with
 * the storage state rather than with the app. Everything up to the click
 * passes, and the only symptom is a bucket control that never appears — which
 * reads exactly like a missing `aria-label` on the control this journey is
 * about. `openArtifacts` therefore judges the navigation's own status code, so
 * a wrong path can never again present itself as a broken component.
 */
const ARTIFACTS_URL = `${BASE_URL}/app/artifacts`;

/** Open the artifacts page and prove the app — not a 404 — is what loaded. */
async function openArtifacts(page: Page): Promise<void> {
  const response = await page.goto(ARTIFACTS_URL);
  expect(response?.status(), 'the artifacts page must be served at /app/artifacts').toBeLessThan(400);
}

/**
 * Enter the route a SECOND time, once the first document has stopped working —
 * the wait the sibling lifecycle spec measured (`reenterArtifacts` there): a
 * navigation started inside the boot traffic is cancelled by WebKit and
 * reported as `page.goto: Frame load interrupted`.
 */
async function reenterArtifacts(page: Page): Promise<void> {
  await page.waitForLoadState('networkidle');
  await openArtifacts(page);
  await page.waitForURL('**/artifacts**', { timeout: 15_000 });
}

/** The project the app itself selected, read from the store the app writes. */
async function selectedProjectId(page: Page): Promise<string> {
  const id = await page.evaluate(() => localStorage.getItem('el.project.id'));
  expect(id, 'app-shell must have persisted a selected project').not.toBeNull();
  return id as string;
}

/** Idempotent backend fixture: the bucket plus one object to read. */
async function seedAclBucket(request: APIRequestContext, projectId: string, bucket: string = ACL_BUCKET): Promise<void> {
  const created = await request.post(`/api/v2/artifacts/buckets/${projectId}`, {
    data: { name: bucket },
  });
  expect([200, 201, 409]).toContain(created.status());

  const uploaded = await request.post(
    `/api/v2/artifacts/objects/${projectId}/${bucket}?overwrite=true`,
    { multipart: { file: { name: ACL_FILE, mimeType: 'text/plain', buffer: Buffer.from(ACL_BODY) } } },
  );
  expect(uploaded.status(), await uploaded.text()).toBe(201);
}

/** Any project member's own database id, from the project member listing. */
async function resolveUserId(request: APIRequestContext, projectId: string, email: string): Promise<number> {
  const response = await request.get(`/api/v2/admin/users/default/${projectId}?limit=100&offset=0`);
  expect(response.status(), await response.text()).toBe(200);
  const body = (await response.json()) as { rows: Array<{ id: string; email: string }> };
  const user = body.rows.find((row) => row.email === email);
  expect(user, `${email} must be a member of project ${projectId}`).toBeDefined();
  return Number((user as { id: string }).id);
}

/** The member persona's own database id, from the project member listing. */
async function memberUserId(request: APIRequestContext, projectId: string): Promise<number> {
  return resolveUserId(request, projectId, MEMBER_EMAIL);
}

/** Remove every exception this spec could have left behind. */
async function clearExceptions(request: APIRequestContext, projectId: string, userId: number): Promise<void> {
  const response = await request.put(`/api/v2/artifacts/bucket_permissions/${projectId}`, {
    data: { user_id: userId, bucket_permissions: {} },
  });
  expect([200, 403]).toContain(response.status());
}

test.describe('J20 artifacts bucket access lists', () => {
  // Serial for the reason the sibling lifecycle spec is: these tests share one
  // bucket and one exception row, and J20j depends on J20i having removed its
  // own.
  test.describe.configure({ mode: 'serial' });

  test('J20h: the dialog states the default and shows no exceptions for an open bucket', async ({ page, request }) => {
    await openArtifacts(page);
    const projectId = await selectedProjectId(page);
    await seedAclBucket(request, projectId);
    const userId = await memberUserId(request, projectId);
    await clearExceptions(request, projectId, userId);

    await reenterArtifacts(page);

    // The control lives in the bucket row's hover slot; `aria-label` is the
    // stable handle, exactly as the pin and delete controls are addressed.
    await page.getByLabel(`Manage access to ${ACL_BUCKET}`).click();

    await expect(page.getByTestId('bucket-access-dialog')).toBeVisible();
    // The sentence is the whole point of the empty state: without it, "no
    // exceptions" and "the read failed" render identically.
    await expect(
      page.getByText('All users have read/write permissions by default.'),
    ).toBeVisible();
    await expect(page.getByTestId('bucket-access-empty')).toBeVisible();
  });

  test('J20i: an exception authored in the dialog is stored and read back', async ({ page, request }) => {
    await openArtifacts(page);
    const projectId = await selectedProjectId(page);
    await seedAclBucket(request, projectId);
    const userId = await memberUserId(request, projectId);
    await clearExceptions(request, projectId, userId);

    await reenterArtifacts(page);
    await page.getByLabel(`Manage access to ${ACL_BUCKET}`).click();
    await expect(page.getByTestId('bucket-access-dialog')).toBeVisible();

    // Pick the member persona in the user field and store a read-only
    // exception. The PUT is awaited so the assertion below judges a settled
    // write rather than an optimistic render.
    await page.getByLabel('Users').click();
    await page.getByRole('option', { name: 'E2E Member' }).click();
    await Promise.all([
      page.waitForResponse(
        (response) =>
          response.url().includes('/api/v2/artifacts/bucket_permissions/') &&
          response.request().method() === 'PUT',
        { timeout: 15_000 },
      ),
      page.getByRole('button', { name: 'Add exception' }).click(),
    ]);

    await expect(page.getByTestId(`bucket-access-row-${userId}`)).toBeVisible();

    // And the SERVER holds it — the screen could render an exception it never
    // sent.
    const stored = await request.get(`/api/v2/artifacts/bucket_permissions/${projectId}`);
    expect(stored.status(), await stored.text()).toBe(200);
    const body = (await stored.json()) as {
      rows: Array<{ user_id: number; bucket_permissions: Record<string, string[]> }>;
    };
    const row = body.rows.find((entry) => entry.user_id === userId);
    expect(row?.bucket_permissions[ACL_BUCKET]).toEqual(['read']);

    await clearExceptions(request, projectId, userId);
  });

  test('J20j: a stored exception changes what the server allows', async ({ page, request }) => {
    await openArtifacts(page);
    const projectId = await selectedProjectId(page);
    await seedAclBucket(request, projectId);
    const userId = await memberUserId(request, projectId);
    await clearExceptions(request, projectId, userId);

    // Baseline: with no exception, the member may write. Without this the test
    // could pass on a bucket that was already unreachable for another reason.
    const beforeUpload = await request.post(
      `/api/v2/artifacts/objects/${projectId}/${ACL_BUCKET}?overwrite=true`,
      { multipart: { file: { name: ACL_FILE, mimeType: 'text/plain', buffer: Buffer.from(ACL_BODY) } } },
    );
    expect(beforeUpload.status(), await beforeUpload.text()).toBe(201);

    // Read-only: the read still works and the write does not.
    const readOnly = await request.put(`/api/v2/artifacts/bucket_permissions/${projectId}`, {
      data: { user_id: userId, bucket_permissions: { [ACL_BUCKET]: ['read'] } },
    });
    expect(readOnly.status(), await readOnly.text()).toBe(200);

    const stillReads = await request.get(`/api/v2/artifacts/objects/${projectId}/${ACL_BUCKET}`);
    expect(stillReads.status(), await stillReads.text()).toBe(200);

    const refusedWrite = await request.post(
      `/api/v2/artifacts/objects/${projectId}/${ACL_BUCKET}?overwrite=true`,
      { multipart: { file: { name: ACL_FILE, mimeType: 'text/plain', buffer: Buffer.from(ACL_BODY) } } },
    );
    expect(refusedWrite.status(), await refusedWrite.text()).toBe(403);

    // No access: even the read is refused, and the bucket leaves the listing.
    const blocked = await request.put(`/api/v2/artifacts/bucket_permissions/${projectId}`, {
      data: { user_id: userId, bucket_permissions: { [ACL_BUCKET]: [] } },
    });
    expect(blocked.status(), await blocked.text()).toBe(200);

    const refusedRead = await request.get(`/api/v2/artifacts/objects/${projectId}/${ACL_BUCKET}`);
    expect(refusedRead.status(), await refusedRead.text()).toBe(403);

    const listing = await request.get(`/api/v2/artifacts/buckets/${projectId}`);
    expect(listing.status()).toBe(200);
    const buckets = (await listing.json()) as { buckets: Array<{ name: string }> };
    expect(buckets.buckets.map((bucket) => bucket.name)).not.toContain(ACL_BUCKET);

    // Restore, and prove the restore worked rather than assuming it.
    await clearExceptions(request, projectId, userId);
    const restored = await request.get(`/api/v2/artifacts/objects/${projectId}/${ACL_BUCKET}`);
    expect(restored.status(), await restored.text()).toBe(200);
  });

  /**
   * Issue 940/A10 (ELITEA-2480) — bulk edit. A bucket OF ITS OWN
   * (`BULK_BUCKET`), same reason `ACL_BUCKET` is: a shared blast radius with
   * J20h-j would make a mid-run failure here corrupt their exception state
   * too.
   */
  test('J20k: bulk edit applies one permission to every selected exception, and Read/write removes them from the table', async ({ page, request }) => {
    const BULK_BUCKET = 'autotest-j20k-bulk-art';
    const ADMIN_EMAIL = 'e2e-admin@autotest.local';
    const VIEWER_EMAIL = 'e2e-viewer@autotest.local';

    await openArtifacts(page);
    const projectId = await selectedProjectId(page);
    await seedAclBucket(request, projectId, BULK_BUCKET);
    const memberId = await resolveUserId(request, projectId, MEMBER_EMAIL);
    const adminId = await resolveUserId(request, projectId, ADMIN_EMAIL);
    const viewerId = await resolveUserId(request, projectId, VIEWER_EMAIL);

    // Seed the case's own starting shape: 3 exceptions (User A/B read-only,
    // User C no access).
    await request.put(`/api/v2/artifacts/bucket_permissions/${projectId}`, {
      data: { user_id: memberId, bucket_permissions: { [BULK_BUCKET]: ['read'] } },
    });
    await request.put(`/api/v2/artifacts/bucket_permissions/${projectId}`, {
      data: { user_id: adminId, bucket_permissions: { [BULK_BUCKET]: ['read'] } },
    });
    await request.put(`/api/v2/artifacts/bucket_permissions/${projectId}`, {
      data: { user_id: viewerId, bucket_permissions: { [BULK_BUCKET]: [] } },
    });

    try {
      await reenterArtifacts(page);
      await page.getByLabel(`Manage access to ${BULK_BUCKET}`).click();
      const dialog = page.getByTestId('bucket-access-dialog');
      await expect(dialog).toBeVisible();
      await expect(dialog.getByText('Exceptions')).toContainText('3');

      // Header checkbox selects, then deselects, every row.
      const selectAll = page.getByLabel('Select all exceptions');
      await selectAll.click();
      await expect(page.getByTestId(`bucket-access-row-${memberId}`).getByRole('checkbox')).toBeChecked();
      await expect(page.getByTestId(`bucket-access-row-${adminId}`).getByRole('checkbox')).toBeChecked();
      await expect(page.getByTestId(`bucket-access-row-${viewerId}`).getByRole('checkbox')).toBeChecked();
      await selectAll.click();
      await expect(page.getByTestId(`bucket-access-row-${memberId}`).getByRole('checkbox')).not.toBeChecked();

      // Select User A + User B (member + admin) only.
      await page.getByTestId(`bucket-access-row-${memberId}`).getByRole('checkbox').click();
      await page.getByTestId(`bucket-access-row-${adminId}`).getByRole('checkbox').click();

      const bulkEditOpen = page.getByTestId('bucket-access-bulk-edit-open');
      await expect(bulkEditOpen).toBeEnabled();
      await bulkEditOpen.click();

      const bulkDialog = page.getByTestId('bucket-access-bulk-edit-dialog');
      await expect(bulkDialog).toBeVisible();
      await expect(bulkDialog.getByText('2 users selected')).toBeVisible();

      await page.getByLabel('Bulk edit permissions value').click();
      await page.getByRole('option', { name: 'No access' }).click();
      await page.getByTestId('bucket-access-bulk-edit-save').click();

      // Bulk edit fires ONE `onSetAccess` write per selected row (two here,
      // member + admin) — both in flight together, not one awaited request.
      // Polled rather than raced against a single `waitForResponse`, which
      // only ever catches whichever of the two lands first.
      const readBucketPermissions = async () => {
        const response = await request.get(`/api/v2/artifacts/bucket_permissions/${projectId}`);
        expect(response.status()).toBe(200);
        return (await response.json()) as { rows: Array<{ user_id: number; bucket_permissions: Record<string, string[]> }> };
      };
      const byId = (rows: Array<{ user_id: number; bucket_permissions: Record<string, string[]> }>, id: number) =>
        rows.find((r) => r.user_id === id)?.bucket_permissions[BULK_BUCKET];

      await expect
        .poll(async () => byId((await readBucketPermissions()).rows, memberId), { timeout: 15_000 })
        .toEqual([]);
      await expect
        .poll(async () => byId((await readBucketPermissions()).rows, adminId), { timeout: 15_000 })
        .toEqual([]);
      // C (never selected) is untouched — still "No access" from the seed.
      expect(byId((await readBucketPermissions()).rows, viewerId)).toEqual([]);

      // Select ALL, bulk edit to "Read & Write" (the default) — every
      // selected row is REMOVED from the exceptions table.
      await page.getByLabel('Select all exceptions').click();
      await page.getByTestId('bucket-access-bulk-edit-open').click();
      await expect(page.getByTestId('bucket-access-bulk-edit-dialog')).toBeVisible();
      await page.getByLabel('Bulk edit permissions value').click();
      await page.getByRole('option', { name: 'Read/write (default)' }).click();
      await page.getByTestId('bucket-access-bulk-edit-save').click();

      // Three rows selected this time — three `onSetAccess` writes in
      // flight together; the empty state only appears once the LAST of
      // them has settled and refetched, so it is itself the poll.
      await expect(page.getByTestId('bucket-access-empty')).toBeVisible({ timeout: 15_000 });
      await expect
        .poll(
          async () => {
            const rows = (await readBucketPermissions()).rows;
            return [memberId, adminId, viewerId].every((id) => byId(rows, id) === undefined);
          },
          { timeout: 15_000, message: 'every selected user must have no exception left on the server' },
        )
        .toBe(true);
    } finally {
      await clearExceptions(request, projectId, memberId);
      await clearExceptions(request, projectId, adminId);
      await clearExceptions(request, projectId, viewerId);
    }
  });
});
