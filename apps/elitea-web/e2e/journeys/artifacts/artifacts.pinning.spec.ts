/**
 * Journey: bucket PINNING in the sidebar — toggle, ordering, persistence,
 * and its interaction with edit/delete/upload.
 *
 * Onetest cases ELITEA-0269/0273/0274/0275/0276/0278/0279/0280/0281/0282/0283
 * describe a legacy "⋮ context menu" with `Pin to top` / `Unpin from top` /
 * `Upload files` / `Edit` / `Delete` menu items. THAT MENU DOES NOT EXIST in
 * this app: `BucketList.tsx` renders Pin, Edit, "Manage access" and Delete as
 * four independent, always-hover-revealed icon buttons on the row itself
 * (`aria-label`s `Pin/Unpin ${name}`, `Edit ${name}`, `Manage access to
 * ${name}`, `Delete ${name}`) — there is no combined popup to open, and so
 * no "Rename"/"Share" item to be absent FROM one; they are simply absent
 * from the row's control set, asserted directly below. Every test therefore
 * ports the BEHAVIOUR each case is actually checking (does pinning expand
 * the row? does the icon and position update? does it survive a reload? do
 * edit/delete/upload keep working?) against the icon-button UI that exists,
 * per PREAMBLE-port.md's "port BY USE CASE, not one-for-one".
 *
 * ELITEA-0277 (pin survives LOGOUT/login) is not ported as its own test: it
 * asserts the same fact ELITEA-0278 (pin survives a page REFRESH) does — that
 * the pin is stored server-side, not only in front-end memory/session — and
 * the rulebook forbids ending the shared session's storage state. See the
 * ledger for the DUP note.
 *
 * Pin state is server truth: `GET/PATCH .../artifacts/buckets/{project}/
 * {bucket}`'s `is_pinned` (verified directly against the running stack —
 * `PATCH` with `{"is_pinned":true|false}` — before writing this file; the
 * client's own `setArtifactBucketPinned` calls the same route). Ordering is
 * `sortBucketsPinnedFirst` (`entities/bucket/model/selectors.ts`): pinned
 * buckets first, otherwise stable — asserted here through each row's DELETE
 * button (its `aria-label` is stable across pin state, unlike the pin
 * button's own label) and its bounding box.
 */
import { test, expect, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

const ARTIFACTS_URL = `${BASE_URL}/app/artifacts`;

/**
 * Reused across the tests in this file (hyphens only, see the sibling J20
 * specs' own note on `BUCKET_NAME_PATTERN`), but RUN-UNIQUE per worker: a
 * fixed name here collided under `--repeat-each=2`, whose two copies of this
 * file's `mode: 'serial'` describe land on two DIFFERENT workers and race on
 * the very same bucket (measured — see PREAMBLE-port.md's "Learned in P1").
 */
const RUN_TOKEN = `${Date.now().toString(36)}${Math.floor(Math.random() * 1000)}`;
const PIN_A = `autotest-pin-a-${RUN_TOKEN}`;
const PIN_B = `autotest-pin-b-${RUN_TOKEN}`;
const PIN_C = `autotest-pin-c-${RUN_TOKEN}`;
const PIN_DELETE = `autotest-pin-delete-${RUN_TOKEN}`;

async function openArtifacts(page: Page): Promise<void> {
  const response = await page.goto(ARTIFACTS_URL);
  expect(response?.status(), 'the artifacts page must be served at /app/artifacts').toBeLessThan(400);
}

async function reenterArtifacts(page: Page, url: string): Promise<void> {
  await page.waitForLoadState('networkidle');
  await page.goto(url);
  await page.waitForURL('**/artifacts**', { timeout: 15_000 });
}

/** The project the app itself selected, read from the store the app writes. */
async function selectedProjectId(page: Page): Promise<string> {
  const id = await page.evaluate(() => localStorage.getItem('el.project.id'));
  expect(id, 'app-shell must have persisted a selected project').not.toBeNull();
  return id as string;
}

async function ensureBucket(request: APIRequestContext, projectId: string, name: string): Promise<void> {
  const created = await request.post(`/api/v2/artifacts/buckets/${projectId}`, { data: { name } });
  expect([200, 201, 409]).toContain(created.status());
}

/** Server truth for a bucket's pin state, read back rather than assumed. */
async function readPinned(request: APIRequestContext, projectId: string, name: string): Promise<boolean> {
  const listing = await request.get(`/api/v2/artifacts/buckets/${projectId}`);
  expect(listing.status(), await listing.text()).toBe(200);
  const body = (await listing.json()) as { buckets: Array<{ name: string; is_pinned: boolean }> };
  const row = body.buckets.find((bucket) => bucket.name === name);
  expect(row, `${name} must exist in the bucket listing`).toBeDefined();
  return (row as { is_pinned: boolean }).is_pinned;
}

async function setPinned(
  request: APIRequestContext,
  projectId: string,
  name: string,
  pinned: boolean,
): Promise<void> {
  const response = await request.patch(`/api/v2/artifacts/buckets/${projectId}/${name}`, {
    data: { is_pinned: pinned },
  });
  expect(response.status(), await response.text()).toBe(200);
}

/** The row's DELETE control — stable across pin state, unlike the pin button's own label. */
function deleteControl(page: Page, name: string) {
  return page.getByLabel(`Delete ${name}`);
}

async function rowTop(page: Page, name: string): Promise<number> {
  const box = await deleteControl(page, name).boundingBox();
  expect(box, `${name}'s row must be on screen`).not.toBeNull();
  return (box as { y: number }).y;
}

/**
 * `rowTop`'s non-throwing twin, for use INSIDE an `expect.poll` callback only.
 * A callback that throws (as `rowTop`'s own `expect(...).not.toBeNull()`
 * does while a row has not yet reappeared post-mutation) does not get the
 * poll's own retry budget here — measured: a poll built from `rowTop`
 * directly failed in ~1s flat, nowhere near its configured timeout, because
 * the throw propagated out of the poll on the very first tick instead of
 * being treated as "not matching yet". Returning `null` keeps the callback
 * itself exception-free so the poll's retries behave as documented.
 */
async function rowTopOrNull(page: Page, name: string): Promise<number | null> {
  const box = await deleteControl(page, name).boundingBox();
  return box?.y ?? null;
}

test.describe('artifacts bucket pinning', () => {
  // Serial: every test in this file shares the same three run-unique buckets
  // and resets their pin state at the top, exactly as the sibling J20/J20-ACL
  // specs do for their own (fixed-name) buckets.
  test.describe.configure({ mode: 'serial' });

  let projectId = '';

  test.beforeAll(async ({ browser }) => {
    const context = await browser.newContext();
    const page = await context.newPage();
    try {
      await openArtifacts(page);
      projectId = await selectedProjectId(page);
      for (const name of [PIN_A, PIN_B, PIN_C]) {
        await ensureBucket(context.request, projectId, name);
        await setPinned(context.request, projectId, name, false);
      }
    } finally {
      await context.close();
    }
  });

  test.afterAll(async ({ browser }) => {
    // Best effort, in its own context: these bucket names are run-unique
    // (see the constants' own note), so a left-behind one is not read back
    // by any later run — but it would sit in the project forever otherwise.
    if (projectId === '') return;
    const context = await browser.newContext();
    try {
      for (const name of [PIN_A, PIN_B, PIN_C, PIN_DELETE]) {
        await context.request.delete(`/api/v2/artifacts/buckets/${projectId}/${name}`).catch(() => undefined);
      }
    } finally {
      await context.close();
    }
  });

  /* onetest: ELITEA-0269 — clicking the pin icon does not expand the bucket; only the name area does */
  test('P1: the pin icon does not select/expand the bucket; the name button does', async ({ page, request }) => {
    await setPinned(request, projectId, PIN_A, false);
    // Land on a DIFFERENT bucket so PIN_A starts collapsed (unselected).
    await reenterArtifacts(page, `${ARTIFACTS_URL}?bucket=${PIN_B}`);
    await expect(page).toHaveURL(new RegExp(`bucket=${PIN_B}`));

    await page.getByLabel(`Pin ${PIN_A}`).click();
    await expect(page.getByLabel(`Unpin ${PIN_A}`)).toBeVisible({ timeout: 10_000 });
    // The click must NOT have navigated the selection to PIN_A.
    await expect(page).toHaveURL(new RegExp(`bucket=${PIN_B}`));

    // The name button DOES select/expand it.
    await page.getByRole('button', { name: PIN_A, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`bucket=${PIN_A}`), { timeout: 10_000 });

    await setPinned(request, projectId, PIN_A, false);
  });

  /* onetest: ELITEA-0279, ELITEA-0282, ELITEA-0283, ELITEA-0275 — the pin icon toggles state, moves the row to the top, and the icon fill is the only pinned/unpinned signal (no "Pinned Buckets" header exists) */
  test('P2: pinning toggles the icon and moves the row to the top; unpinning reverts both', async ({
    page,
    request,
  }) => {
    await setPinned(request, projectId, PIN_A, false);
    await setPinned(request, projectId, PIN_C, false);
    await reenterArtifacts(page, ARTIFACTS_URL);
    await expect(page.getByText('Pinned Buckets', { exact: false })).toHaveCount(0);

    // PIN_A stays unpinned for the whole test and sorts BEFORE "autotest-pin-c"
    // in every ordering except pin state (name, and — measured against the
    // running stack — this list's own incidental order). Pinning PIN_C and
    // watching it overtake still-unpinned PIN_A is therefore a fact that can
    // only be `isPinned`, not a coincidence of row order.
    const aBefore = await rowTop(page, PIN_A);
    const cBefore = await rowTop(page, PIN_C);
    expect(cBefore, 'PIN_C must start unpinned, i.e. not already above PIN_A').toBeGreaterThanOrEqual(aBefore);

    await page.getByLabel(`Pin ${PIN_C}`).click();
    // The aria-label flip IS the pinned/unpinned signal this control exposes
    // (`Unpin ${name}` <-> `Pin ${name}`, `BucketList.tsx`); the icon itself
    // (`PushPinIcon` filled vs `PushPinOutlined`) renders from the same
    // `bucket.isPinned` boolean, so the label is not a weaker proxy for it.
    await expect(page.getByLabel(`Unpin ${PIN_C}`)).toBeVisible({ timeout: 10_000 });
    await expect.poll(() => readPinned(request, projectId, PIN_C)).toBe(true);

    const cPinned = await rowTop(page, PIN_C);
    expect(cPinned, 'a newly pinned row must sit ABOVE a still-unpinned one').toBeLessThan(await rowTop(page, PIN_A));

    await page.getByLabel(`Unpin ${PIN_C}`).click();
    await expect(page.getByLabel(`Pin ${PIN_C}`)).toBeVisible({ timeout: 10_000 });
    await expect.poll(() => readPinned(request, projectId, PIN_C)).toBe(false);
  });

  /* onetest: ELITEA-0276 — multiple pinned buckets group at the top; unpinning one does not move the others */
  test('P3: pinning several buckets groups them at the top; unpinning one leaves the rest in place', async ({
    page,
    request,
  }) => {
    // The shared project's bucket list (every onetest-port spec file's own
    // buckets accumulate here) has been measured well over 70 rows — the
    // sidebar's OWN "Loading buckets…" placeholder can outlast the default
    // 30s test timeout while this test's two ordering polls are still
    // waiting their turn, well before either poll's own budget is spent.
    test.setTimeout(60_000);
    for (const name of [PIN_A, PIN_B, PIN_C]) await setPinned(request, projectId, name, false);
    await reenterArtifacts(page, ARTIFACTS_URL);

    // Each click's label flip is the per-row cache write; the ROW ORDER
    // reflects `sortBucketsPinnedFirst` over the LIST query's own refetch
    // (`onSuccess: refreshBuckets` invalidating it), which can land a beat
    // after the label does. This used to anchor each click on a
    // `page.waitForResponse` for that GET — measured flaky under load: a
    // second (or third) `invalidateQueries` call while the first GET this
    // triggered is still in flight is served off that SAME in-flight fetch
    // rather than issuing a new network request, so a `waitForResponse`
    // registered per click can legitimately see no new response at all and
    // time out even though the mutation succeeded and the order did update.
    // Poll the actual rendered order instead of a specific network event.
    for (const name of [PIN_A, PIN_B, PIN_C]) {
      await page.getByLabel(`Pin ${name}`).click();
      await expect(page.getByLabel(`Unpin ${name}`)).toBeVisible({ timeout: 10_000 });
    }

    // All three pinned rows are consecutive AND stable-ordered (A < B < C,
    // the order they were pinned in — `sortBucketsPinnedFirst` is a stable
    // sort on `isPinned` alone).
    await expect
      .poll(
        async () => {
          const [a, b, c] = [
            await rowTopOrNull(page, PIN_A),
            await rowTopOrNull(page, PIN_B),
            await rowTopOrNull(page, PIN_C),
          ];
          return a !== null && b !== null && c !== null && a < b && b < c;
        },
        { timeout: 20_000, message: 'all three pinned rows must end up consecutive and in pin order (A < B < C)' },
      )
      .toBe(true);

    await page.getByLabel(`Unpin ${PIN_B}`).click();
    await expect(page.getByLabel(`Pin ${PIN_B}`)).toBeVisible({ timeout: 10_000 });

    // A and C, both still pinned, keep their relative order; B (no longer
    // pinned) leaves the pinned group and drops below C.
    await expect
      .poll(
        async () => {
          const [afterA, afterB, afterC] = [
            await rowTopOrNull(page, PIN_A),
            await rowTopOrNull(page, PIN_B),
            await rowTopOrNull(page, PIN_C),
          ];
          return (
            afterA !== null && afterB !== null && afterC !== null && afterA < afterC && afterB > afterC
          );
        },
        { timeout: 20_000, message: 'unpinning B must leave A/C order intact and move B out of the pinned group' },
      )
      .toBe(true);

    for (const name of [PIN_A, PIN_C]) await setPinned(request, projectId, name, false);
  });

  /* onetest: ELITEA-0278, DUP ELITEA-0277 (same server-side-persistence claim; see file header) — pin state is server truth and survives a reload */
  test('P4: pin state survives a full page reload', async ({ page, request }) => {
    await setPinned(request, projectId, PIN_A, true);
    await reenterArtifacts(page, ARTIFACTS_URL);
    await expect(page.getByLabel(`Unpin ${PIN_A}`)).toBeVisible({ timeout: 10_000 });

    await page.reload();
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });
    await expect(page.getByLabel(`Unpin ${PIN_A}`)).toBeVisible({ timeout: 15_000 });

    await setPinned(request, projectId, PIN_A, false);
  });

  /* onetest: ELITEA-0274 — Edit on a pinned bucket: name is read-only, retention is editable, and the bucket stays pinned */
  test('P5: editing a pinned bucket keeps it pinned; the name field is read-only', async ({ page, request }) => {
    await setPinned(request, projectId, PIN_A, true);
    await reenterArtifacts(page, ARTIFACTS_URL);
    await expect(page.getByLabel(`Unpin ${PIN_A}`)).toBeVisible({ timeout: 10_000 });

    await page.getByLabel(`Edit ${PIN_A}`).click();
    await page.waitForURL('**/create-bucket**', { timeout: 10_000 });

    const nameField = page.getByLabel('Name');
    await expect(nameField).toHaveValue(PIN_A);
    await expect(nameField).toHaveAttribute('readonly', '');

    await page.getByLabel('Retention (days)').fill('30');
    await page.getByRole('button', { name: 'Save bucket' }).click();
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });

    await expect(page.getByLabel(`Unpin ${PIN_A}`)).toBeVisible({ timeout: 10_000 });
    await expect.poll(() => readPinned(request, projectId, PIN_A)).toBe(true);

    await setPinned(request, projectId, PIN_A, false);
  });

  /* onetest: ELITEA-0273, ELITEA-0281 — a bucket row exposes exactly Pin/Edit/Manage access/Delete (no Rename, no Share), and upload/delete both keep working once the bucket is pinned */
  test('P6: no Rename/Share controls exist on a bucket row, and pinning does not break upload or delete', async ({
    page,
    request,
  }) => {
    await setPinned(request, projectId, PIN_A, false);
    const fileName = 'pin-upload-art.txt';
    // Idempotent: a previous, differently-failed run may have left this
    // object behind, which would turn the plain upload below into a
    // duplicate-conflict flow this test does not drive.
    await request.post(`/api/v2/artifacts/objects/${projectId}/${PIN_A}:batchDelete`, {
      data: { keys: [fileName] },
    }).catch(() => undefined);
    await reenterArtifacts(page, ARTIFACTS_URL);

    await expect(page.getByLabel(`Rename ${PIN_A}`)).toHaveCount(0);
    await expect(page.getByLabel(`Share ${PIN_A}`)).toHaveCount(0);
    for (const label of ['Pin', 'Edit', 'Manage access to', 'Delete']) {
      await expect(page.getByLabel(new RegExp(`^${label}`))).not.toHaveCount(0);
    }

    await page.getByLabel(`Pin ${PIN_A}`).click();
    await expect(page.getByLabel(`Unpin ${PIN_A}`)).toBeVisible({ timeout: 10_000 });

    await page.getByRole('button', { name: PIN_A, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`bucket=${PIN_A}`), { timeout: 10_000 });

    await page.locator('input[type="file"]').setInputFiles({
      name: fileName,
      mimeType: 'text/plain',
      buffer: Buffer.from('pinned bucket upload'),
    });
    await page.getByRole('button', { name: 'Continue', exact: true }).click();
    await expect(page.getByRole('row').filter({ hasText: fileName })).toBeVisible({ timeout: 20_000 });
    // Pinning must not have been disturbed by the upload.
    await expect(page.getByLabel(`Unpin ${PIN_A}`)).toBeVisible();

    await page.getByLabel(`Select ${fileName}`).click();
    await page.getByRole('button', { name: 'Delete selected' }).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Delete', exact: true }).click();
    await expect(page.getByRole('row').filter({ hasText: fileName })).toHaveCount(0, { timeout: 15_000 });
    await expect(page.getByLabel(`Unpin ${PIN_A}`)).toBeVisible();

    await setPinned(request, projectId, PIN_A, false);
  });

  /* onetest: ELITEA-0280 — deleting a pinned bucket removes it entirely (no ghost row) and the app auto-selects another bucket */
  test('P7: deleting a pinned bucket removes it completely and auto-selects another bucket', async ({
    page,
    request,
  }) => {
    await ensureBucket(request, projectId, PIN_DELETE);
    await setPinned(request, projectId, PIN_DELETE, true);
    await reenterArtifacts(page, `${ARTIFACTS_URL}?bucket=${PIN_DELETE}`);
    await expect(page.getByLabel(`Unpin ${PIN_DELETE}`)).toBeVisible({ timeout: 10_000 });

    await page.getByLabel(`Delete ${PIN_DELETE}`).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Delete', exact: true }).click();

    await expect(page.getByLabel(`Unpin ${PIN_DELETE}`)).toHaveCount(0, { timeout: 15_000 });
    await expect(page.getByLabel(`Pin ${PIN_DELETE}`)).toHaveCount(0);
    await expect(page.getByRole('button', { name: PIN_DELETE, exact: true })).toHaveCount(0);

    // Auto-selection: `search.bucket` cleared by the delete, then re-filled
    // by `Artifacts.tsx`'s own effect with the FIRST remaining bucket — never
    // left on `bucket=` (blank/undefined).
    await expect
      .poll(() => new URL(page.url()).searchParams.get('bucket'), { timeout: 15_000 })
      .not.toBe(null);
    await expect
      .poll(() => new URL(page.url()).searchParams.get('bucket'), { timeout: 15_000 })
      .not.toBe('');

    const listing = await request.get(`/api/v2/artifacts/buckets/${projectId}`);
    expect(listing.status()).toBe(200);
    const names = ((await listing.json()) as { buckets: Array<{ name: string }> }).buckets.map((b) => b.name);
    expect(names).not.toContain(PIN_DELETE);
  });
});
