/**
 * Journey: the upload PATH dialog's conflict handling — when the duplicate
 * warning appears, and what "replace" actually does to the stored object.
 *
 * The onetest cases describe a single dialog that grows an inline
 * "Proceed"-style conflict warning as soon as a colliding destination path is
 * typed. The real flow (`useArtifactUpload.ts`) is two dialogs: choosing a
 * file opens `UploadPathDialog` (just the destination folder field, no
 * conflict UI at all — `confirmPath` is not called until "Continue" is
 * clicked), and ONLY THEN, if `buildArtifactUploadPlan` finds the target key
 * already taken, does a SEPARATE `DuplicateResolutionDialog` ("Files already
 * exist") open with Cancel / Skip duplicates / Keep both / Replace. So the
 * case's core claim — no conflict is shown merely for picking a file or
 * typing a path, only once a colliding destination is actually confirmed —
 * still holds; "Continue" stands in for the case's "Proceed", and "Replace"
 * for its overwrite confirmation. Verified against `UploadPathDialog.tsx`
 * and `DuplicateResolutionDialog.tsx` before writing this file.
 */
import { test, expect, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

const ARTIFACTS_URL = `${BASE_URL}/app/artifacts`;
// Run-unique: `--repeat-each=2` runs two copies of this file's serial suite
// on two different workers, which race on a FIXED bucket name (measured —
// see PREAMBLE-port.md's "Learned in P1").
const RUN_TOKEN = `${Date.now().toString(36)}${Math.floor(Math.random() * 1000)}`;
const BUCKET = `autotest-upload-conflict-${RUN_TOKEN}`;
const REPORT = 'report.txt';
const V1_BODY = 'version 1 content';
const V2_BODY = 'version 2 content';

async function openArtifacts(page: Page): Promise<void> {
  const response = await page.goto(ARTIFACTS_URL);
  expect(response?.status(), 'the artifacts page must be served at /app/artifacts').toBeLessThan(400);
}

async function reenterArtifacts(page: Page, url: string): Promise<void> {
  await page.waitForLoadState('networkidle');
  await page.goto(url);
  await page.waitForURL('**/artifacts**', { timeout: 15_000 });
}

async function selectedProjectId(page: Page): Promise<string> {
  const id = await page.evaluate(() => localStorage.getItem('el.project.id'));
  expect(id, 'app-shell must have persisted a selected project').not.toBeNull();
  return id as string;
}

/** Idempotent: the bucket, `folder-a/report.txt` at v1, and NO object at `folder-b/report.txt`. */
async function seedFixture(request: APIRequestContext, projectId: string): Promise<void> {
  const created = await request.post(`/api/v2/artifacts/buckets/${projectId}`, { data: { name: BUCKET } });
  expect([200, 201, 409]).toContain(created.status());

  const seeded = await request.post(`/api/v2/artifacts/objects/${projectId}/${BUCKET}?overwrite=true`, {
    multipart: { file: { name: `folder-a/${REPORT}`, mimeType: 'text/plain', buffer: Buffer.from(V1_BODY) } },
  });
  expect(seeded.status(), await seeded.text()).toBe(201);

  // A previous run's "clean path" half may have left an object here — clear
  // it so `folder-b` is genuinely conflict-free for this run too.
  await request
    .post(`/api/v2/artifacts/objects/${projectId}/${BUCKET}:batchDelete`, {
      data: { keys: [`folder-b/${REPORT}`] },
    })
    .catch(() => undefined);
}

async function pickFile(page: Page, body: string): Promise<void> {
  await page.locator('input[type="file"]').setInputFiles({
    name: REPORT,
    mimeType: 'text/plain',
    buffer: Buffer.from(body),
  });
}

test.describe('artifacts upload: destination-path conflicts', () => {
  test.describe.configure({ mode: 'serial' });
  let projectId = '';

  test.beforeAll(async ({ browser }) => {
    const context = await browser.newContext();
    const page = await context.newPage();
    try {
      await openArtifacts(page);
      projectId = await selectedProjectId(page);
      await seedFixture(context.request, projectId);
    } finally {
      await context.close();
    }
  });

  test.afterAll(async ({ browser }) => {
    if (projectId === '') return;
    const context = await browser.newContext();
    try {
      await context.request.delete(`/api/v2/artifacts/buckets/${projectId}/${BUCKET}`).catch(() => undefined);
    } finally {
      await context.close();
    }
  });

  /* onetest: ELITEA-0285 — no duplicate warning on file selection; it appears only once a colliding destination is confirmed, and a clean destination shows none */
  test('U1: the duplicate dialog is absent until Continue is clicked against a colliding path', async ({
    page,
    request,
  }) => {
    await seedFixture(request, projectId);
    await reenterArtifacts(page, `${ARTIFACTS_URL}?bucket=${BUCKET}`);

    await pickFile(page, 'throwaway — never uploaded');
    await expect(page.getByRole('dialog').getByLabel('Additional folder path')).toBeVisible({ timeout: 10_000 });
    // Nothing about duplicates yet — the path has not even been typed.
    await expect(page.getByText('Files already exist')).toHaveCount(0);

    await page.getByLabel('Additional folder path').fill('folder-a');
    // Still nothing: `confirmPath` (where the duplicate check runs) has not
    // been invoked, only the field's own validation has.
    await expect(page.getByText('Files already exist')).toHaveCount(0);

    await page.getByRole('button', { name: 'Continue', exact: true }).click();
    const duplicateDialog = page.getByRole('dialog').filter({ hasText: 'Files already exist' });
    await expect(duplicateDialog).toBeVisible({ timeout: 10_000 });
    await expect(duplicateDialog.getByText(REPORT, { exact: true })).toBeVisible();
    await duplicateDialog.getByRole('button', { name: 'Cancel', exact: true }).click();
    await expect(duplicateDialog).toBeHidden({ timeout: 10_000 });

    // The complement: a clean destination shows no duplicate dialog at all.
    await pickFile(page, 'clean destination upload');
    await page.getByLabel('Additional folder path').fill('folder-b');
    await page.getByRole('button', { name: 'Continue', exact: true }).click();
    await expect(page.getByText('Files already exist')).toHaveCount(0);

    await expect
      .poll(
        async () => {
          const listing = await request.get(`/api/v2/artifacts/objects/${projectId}/${BUCKET}`);
          const body = (await listing.json()) as { objects?: Array<{ key?: string }> };
          return (body.objects ?? []).some((object) => object.key === `folder-b/${REPORT}`);
        },
        { timeout: 15_000 },
      )
      .toBe(true);

    await request
      .post(`/api/v2/artifacts/objects/${projectId}/${BUCKET}:batchDelete`, {
        data: { keys: [`folder-b/${REPORT}`] },
      })
      .catch(() => undefined);
  });

  /* onetest: ELITEA-0284 — clicking Replace in the conflict dialog still overwrites the file end-to-end, in one click with no further prompts */
  test('U2: Replace overwrites the existing object, with no further prompts', async ({ page, request }) => {
    await seedFixture(request, projectId);
    await reenterArtifacts(page, `${ARTIFACTS_URL}?bucket=${BUCKET}`);

    await pickFile(page, V2_BODY);
    await page.getByLabel('Additional folder path').fill('folder-a');
    await page.getByRole('button', { name: 'Continue', exact: true }).click();

    const duplicateDialog = page.getByRole('dialog').filter({ hasText: 'Files already exist' });
    await expect(duplicateDialog).toBeVisible({ timeout: 10_000 });
    await duplicateDialog.getByRole('button', { name: 'Replace', exact: true }).click();

    // No further prompt of any kind — both dialogs are gone.
    await expect(duplicateDialog).toBeHidden({ timeout: 10_000 });
    await expect(page.getByRole('dialog')).toHaveCount(0);

    // The overwrite reached the store — read it back rather than the screen.
    await expect
      .poll(async () => {
        const stored = await request.get(`/api/v2/artifacts/objects/${projectId}/${BUCKET}/folder-a/${REPORT}`);
        return stored.ok() ? stored.text() : undefined;
      }, { timeout: 15_000 })
      .toBe(V2_BODY);

    // And the preview shows it too, not only the API.
    await page.goto(`${ARTIFACTS_URL}?bucket=${BUCKET}&file=folder-a%2F${REPORT}`);
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });
    await expect(page.getByText(V2_BODY)).toBeVisible({ timeout: 15_000 });

    // Restore v1 so a re-run's seed step is a plain overwrite, not a fixture
    // this test itself silently depends on.
    const restored = await request.post(`/api/v2/artifacts/objects/${projectId}/${BUCKET}?overwrite=true`, {
      multipart: { file: { name: `folder-a/${REPORT}`, mimeType: 'text/plain', buffer: Buffer.from(V1_BODY) } },
    });
    expect(restored.status(), await restored.text()).toBe(201);
  });
});
