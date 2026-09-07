/**
 * Journey 20: Artifacts: create bucket → upload → preview → download → ZIP multi-download → delete (JRNY-020)
 *
 * Spec §8.5 acceptance (from parity/manifest/artifacts.json JRNY-020).
 * Acceptance: each step behaves as in the baseline including the direct storage upload;
 * errors at any step are surfaced without corrupting the bucket view.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * STATE OF THIS JOURNEY (verified against the running E2E stack, 2026-08-08)
 *
 * All six tests pass on chromium and webkit. J20b-J20e carried
 * `test.fail()` until issue #138 was fixed: the client addressed the LEGACY
 * Pylon artifact URLs (`/artifacts/s3/...`, `/artifacts/artifact/default/...`,
 * `/artifacts/buckets/default/{projectId}`) while elitea-main serves only
 * `/api/v2/artifacts/{buckets,objects,grants}` (`mountArtifactRoutes`,
 * router.go:255-311), so no bucket, file table, preview, download or ZIP was
 * reachable at all; and a collapsing `FormHelperText` on the create-bucket
 * form moved the button row 22.9 px on the blur that the first click itself
 * produced, so that click never landed.
 *
 * Artifacts are ENABLED in the E2E stack — `ELITEA_ARTIFACTS_ENABLED` is only
 * read at `services/elitea-main/cmd/elitea-main/main.go:103` (`!= "false"`)
 * and `deploy/docker-compose.e2e-standalone.yml` never sets it, so
 * elitea-main boots with a live rustfs-backed object store.
 *
 * JRNY-020's final "delete" step IS asserted, in J20d. It used to be
 * unassertable: `apps/elitea-web/scripts/e2e-stack.sh` seeded only
 * `configuration.artifacts.{artifacts,buckets}.{create,view}`, never `.edit`
 * or `.delete`, so every destructive artifact route 403'd for both personas —
 * an E2E-stack gap, not an app defect. The seed now grants all four
 * `configuration.artifacts.artifacts.*` strings that
 * `mountArtifactRoutes` (router.go:255-262) gates on; measured before/after,
 * DELETE /api/v2/artifacts/buckets/1/<name> went 403 -> 204.
 *
 * The two buckets this spec touches still use FIXED names and are reused
 * across runs (rather than one per run): `seedBucketWithFiles` is idempotent
 * and re-uploads the objects J20d deletes, so J20e still finds them.
 *
 * J20f was added with #194: `GET /elitea_core/chat_config/prompt_lib/
 * {projectID}` — the only server input this feature has — answered 404 in
 * every deployment, and no journey covered it, so every upload silently used
 * the client's 150 MB fallback instead of the project's configured limit.
 * ─────────────────────────────────────────────────────────────────────────────
 */
import { test, expect, type APIRequestContext, type Page } from '@playwright/test';
import JSZip from 'jszip';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';

/**
 * Fixed, reused bucket names. `AUTOTEST_PREFIX` ('autotest_') is deliberately
 * NOT used: `CreateBucket.tsx:17`'s `BUCKET_NAME_PATTERN`
 * (/^[a-zA-Z][a-zA-Z0-9-]*$/) rejects the underscore, which is what left the
 * previous revision of this spec clicking a permanently-disabled submit
 * button for 30 s. `autotest-` keeps the sweepable prefix within the S3 naming
 * rules the app (correctly) enforces. `-art` suffix per the run convention.
 */
const READ_BUCKET = 'autotest-j20-art';
const FORM_BUCKET = 'autotest-j20-form-art';
const FILE_NAME = 'j20-art.txt';
/** 25 bytes — the table must render "25 B", a value only the backend can supply. */
const FILE_BODY = 'E2E artifact test content';
const SECOND_FILE_NAME = 'j20-second-art.txt';
const SECOND_FILE_BODY = 'second artifact for the ZIP';

/** The project the app itself selected, read from the store the app writes. */
async function selectedProjectId(page: Page): Promise<string> {
  const id = await page.evaluate(() => localStorage.getItem('el.project.id'));
  expect(id, 'app-shell must have persisted a selected project').not.toBeNull();
  return id as string;
}

/** Idempotent backend fixture: bucket + two objects with known bytes. */
async function seedBucketWithFiles(request: APIRequestContext, projectId: string): Promise<void> {
  // 200 on create, 409 when a previous run already created it — both mean
  // "the bucket exists", which is the only postcondition this helper promises.
  const created = await request.post(`/api/v2/artifacts/buckets/${projectId}`, {
    data: { name: READ_BUCKET },
  });
  expect([200, 201, 409]).toContain(created.status());

  // `overwrite=true` (objects.go:301) so a re-run replaces the object instead
  // of 409-ing — this spec cannot delete anything (see the header note).
  for (const [name, body] of [[FILE_NAME, FILE_BODY], [SECOND_FILE_NAME, SECOND_FILE_BODY]] as const) {
    const uploaded = await request.post(`/api/v2/artifacts/objects/${projectId}/${READ_BUCKET}?overwrite=true`, {
      multipart: { file: { name, mimeType: 'text/plain', buffer: Buffer.from(body) } },
    });
    expect(uploaded.status(), await uploaded.text()).toBe(201);
    expect((await uploaded.json()).size_bytes).toBe(body.length);
  }
}

/** Drain a Playwright download into a Buffer. */
async function readDownload(download: { createReadStream: () => Promise<NodeJS.ReadableStream> }): Promise<Buffer> {
  const stream = await download.createReadStream();
  const chunks: Buffer[] = [];
  for await (const chunk of stream) chunks.push(chunk as Buffer);
  return Buffer.concat(chunks);
}

test.describe('J20 artifacts lifecycle', () => {
  /*
   * Serial, because these five tests share two FIXED bucket names and a fixed
   * set of object keys (see the header note). That design is deliberate, but it
   * only works in a defined order: J20d DELETES `j20-art.txt`, and the header's
   * own reasoning — "seedBucketWithFiles is idempotent and re-uploads the
   * objects J20d deletes, so J20e still finds them" — is a statement about
   * sequence. The root config sets `fullyParallel: true`, which parallelises
   * tests WITHIN a file too, so on CI's 4 workers J20d and J20e raced over the
   * same object: J20d's download and its :batchDelete both timed out, at a
   * different point on each of the three attempts.
   *
   * Invisible locally because the documented local command is `--workers=1`,
   * which serialises the file by accident. This makes the ordering the spec
   * already depends on explicit instead of accidental. No assertion changes.
   */
  test.describe.configure({ mode: 'serial' });

  /**
   * The create-bucket screen is a real form, not scaffolding.
   *
   * Every assertion here is something a stub page cannot satisfy: the Name
   * field arrives PREFILLED with `new-bucket` (CreateBucket.tsx:32), and the
   * live validator (CreateBucket.tsx:19-26) both disables submit and prints
   * its exact message for a name that violates the S3 bucket-name rule.
   * Deliberately no `getByRole('heading')` anywhere — a bare heading is
   * precisely what a stub renders.
   */
  test('J20a: the create-bucket form is real — prefilled name and live validation', async ({ page }) => {
    await page.goto(BASE_URL + '/app/artifacts');
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });
    await checkA11y(page);

    // Production affordance: BucketSidebar's icon button (BucketSidebar.tsx:58).
    await page.getByRole('button', { name: /^create bucket$/i }).first().click();
    await page.waitForURL('**/artifacts/create-bucket**', { timeout: 15_000 });

    const name = page.getByRole('textbox', { name: /^name$/i });
    await expect(name).toHaveValue('new-bucket');
    await expect(name).toHaveAttribute('maxlength', '56');

    const submit = page.getByRole('button', { name: /^create bucket$/i });
    await expect(submit).toBeEnabled();

    // A name that breaks the pattern must disable submit AND print the
    // validator's exact sentence. A stub renders neither.
    await name.fill('9-starts-with-a-digit');
    await name.blur();
    await expect(page.getByText('Start with a letter and use only letters, numbers, and hyphens.')).toBeVisible();
    await expect(submit).toBeDisabled();

    // …and recovering restores it, proving the validator is live, not a
    // one-shot render.
    await name.fill(FORM_BUCKET);
    await expect(submit).toBeEnabled();
    await checkA11y(page);
  });

  /**
   * A single click on "Create bucket" must submit the form.
   *
   * The regression this guards: `CreateBucket.tsx`'s `helperText` used to
   * become '' on blur, collapsing FormHelperText and moving the button row up
   * 22.9 px (measured: y=204.03 -> y=181.13). The user's very first click
   * after typing IS the blur, so mousedown landed on the button and mouseup
   * 23 px below it — no `click` event, no submit, no feedback (#138).
   */
  test('J20b: one click on Create bucket submits the form', async ({ page, request }) => {
    await page.goto(BASE_URL + '/app/artifacts');
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });
    const projectId = await selectedProjectId(page);

    await page.getByRole('button', { name: /^create bucket$/i }).first().click();
    await page.waitForURL('**/artifacts/create-bucket**', { timeout: 15_000 });

    const name = page.getByRole('textbox', { name: /^name$/i });
    await name.fill(FORM_BUCKET);

    // Exactly one click — the whole point of the test. No blur() first.
    //
    // waitForRESPONSE, not waitForRequest: the next step lists the buckets from
    // the API, and "the request was issued" says nothing about the row having
    // been written. Observed twice locally — the POST fired, the listing came
    // back `[]`, and the failure read as if the click had not landed, which is
    // the exact symptom this test exists to detect (#138). CI's two retries
    // were absorbing it. Waiting for the server's own answer removes the race
    // without weakening anything: the click assertion is unchanged and the
    // status is now checked too.
    const submitted = page.waitForResponse(
      (res) => res.request().method() === 'POST'
        && res.url().endsWith(`/api/v2/artifacts/buckets/${projectId}`),
      { timeout: 10_000 },
    );
    await page.getByRole('button', { name: /^create bucket$/i }).click();
    const created = await submitted;
    expect([200, 201, 409], await created.text()).toContain(created.status());

    // Backend-derived confirmation: the bucket is in the API's own list.
    const listed = await request.get(`/api/v2/artifacts/buckets/${projectId}`);
    expect(listed.status()).toBe(200);
    expect((await listed.json()).buckets.map((b: { name: string }) => b.name)).toContain(FORM_BUCKET);
  });

  /**
   * The bucket sidebar must list the buckets the backend actually holds.
   *
   * The assertions are backend-derived on purpose: a bucket name that only
   * exists because this test POSTed it, addressed through the row's own
   * `aria-label="Delete <name>"` (BucketSidebar.tsx). A stub page cannot
   * produce either. Before #138 this listed nothing at all — the client asked
   * `/artifacts/s3/?project_id=…` (404) and
   * `/artifacts/buckets/default/{projectId}` (403, the literal `default`
   * eaten as `{projectID}`).
   */
  test('J20c: the sidebar lists a bucket that exists on the backend', async ({ page, request }) => {
    await page.goto(BASE_URL + '/app/artifacts');
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });
    const projectId = await selectedProjectId(page);
    await seedBucketWithFiles(request, projectId);

    /*
     * Let the first load FINISH before asking for a second one.
     *
     * `page.goto` resolves on `load`, and this app keeps working after that: it
     * pulls ~50 lazy route chunks and issues its boot calls (`/forward-auth/
     * info`, `/social/author`, the permission list, the project list). Reloading
     * into that traffic made WebKit cancel its own fresh navigation and start it
     * again, which Playwright reports on the FIRST attempt as
     * `page.reload: Frame load interrupted` — the CI failure this line repairs.
     * The page always recovered; only the call's first navigation was lost.
     *
     * Measured on the E2E stack, webkit, 24 runs each: reload-while-booting
     * failed 9 times and a plain second `goto` 13 times; waiting first failed 0.
     * The wait costs 0.7-1.6 s and settles well before the default timeout —
     * the notification SSE stream stays open across it and does not hold it.
     *
     * No assertion is weakened: the three below still judge a FRESH document.
     */
    await page.waitForLoadState('networkidle');

    await page.reload();
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });

    await expect(page.getByRole('button', { name: `Delete ${READ_BUCKET}`, exact: true })).toBeVisible({ timeout: 15_000 });
    await expect(page.getByRole('button', { name: `Pin ${READ_BUCKET}`, exact: true })).toBeVisible();
    // Symptom check, kept last so it never masks the two assertions above.
    await expect(page.getByText('Failed to load buckets.')).toHaveCount(0);
  });

  /**
   * Open the bucket, see the uploaded file with its real size, download it,
   * and get the exact bytes back.
   *
   * The object is seeded through the API here; the UI is only asked to READ
   * it, which is the weakest possible form of this assertion. "25 B" is
   * `formatArtifactSize` (entities/artifact/model/selectors.ts:11) applied to
   * the backend's own `size_bytes`, and the download assertion compares the
   * file's real content — neither is renderable by a stub.
   */
  test('J20d: the file table shows the real size and Download returns the real bytes', async ({ page, request }) => {
    await page.goto(BASE_URL + '/app/artifacts');
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });
    const projectId = await selectedProjectId(page);
    await seedBucketWithFiles(request, projectId);

    await page.goto(`${BASE_URL}/app/artifacts?bucket=${READ_BUCKET}`);
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });

    // The row itself, addressed by the file's own name.
    const row = page.getByRole('row').filter({ hasText: FILE_NAME });
    await expect(row).toBeVisible({ timeout: 15_000 });
    await expect(row).toContainText(`${FILE_BODY.length} B`);

    // Preview pane opens on the file's own preview affordance
    // (ArtifactTable.tsx:280) and renders the file's actual text.
    await page.getByRole('button', { name: `Preview ${FILE_NAME}`, exact: true }).click();
    await expect(page.getByText(FILE_BODY)).toBeVisible({ timeout: 15_000 });
    await checkA11y(page);

    // Download must deliver the exact bytes that were uploaded.
    //
    // Re-enter the bucket by URL rather than `page.goBack()`: the step needs
    // the file table, not the previous history entry, and waiting for the row
    // states that precondition instead of assuming the view has settled.
    await page.goto(`${BASE_URL}/app/artifacts?bucket=${READ_BUCKET}`);
    await expect(row).toBeVisible({ timeout: 15_000 });
    const [download] = await Promise.all([
      page.waitForEvent('download', { timeout: 15_000 }),
      page.getByRole('button', { name: `Download ${FILE_NAME}`, exact: true }).click(),
    ]);
    expect(download.suggestedFilename()).toBe(FILE_NAME);
    expect((await readDownload(download)).toString()).toBe(FILE_BODY);

    // Dismiss the MUI tooltip the Download button just opened, before touching
    // the Delete button sitting immediately to its right.
    //
    // This is what made J20d fail on BOTH engines in CI, deterministically, on
    // all three attempts (issue #154, runs 31329658116 and 31330333209). The
    // failure snapshot ends with:
    //
    //     - tooltip "Download" [ref=f2e274]
    //
    // — a portal-rendered popper still mounted over its neighbour, with
    // `Download j20-art.txt` still `[active]` and no confirm dialog open. The
    // Delete click never passed Playwright's hit-target check, so it retried
    // silently until the :batchDelete wait timed out, and the timeout error
    // pointed at the response wait rather than at the click that was stuck.
    //
    // Invisible locally: the pointer ends up elsewhere and the tooltip has
    // closed by the time the next click is issued. Moving the pointer off the
    // control and waiting for the popper to unmount is what the app's own user
    // does implicitly.
    await page.mouse.move(0, 0);
    await expect(page.getByRole('tooltip')).toBeHidden({ timeout: 15_000 });

    // ── JRNY-020's final "delete" step, now that the E2E seed grants
    // `configuration.artifacts.artifacts.delete` (e2e-stack.sh) and the route
    // no longer 403s. Driven entirely through the UI: the row's own Delete
    // affordance (ArtifactTable.tsx:295-300) opens the confirm dialog
    // (Artifacts.tsx:298-314), and only the dialog's Delete issues the call.
    // The wire call is POST `.../objects/{projectID}/{bucket}:batchDelete`
    // (keys in a JSON body), NOT a DELETE verb — `removeArtifacts`
    // (artifactsApi.ts:146-149) routes every deletion, single or multi,
    // through the batch endpoint.
    const [deleteResp] = await Promise.all([
      page.waitForResponse((r) => /\/api\/v2\/artifacts\/objects\/.*:batchDelete/.test(r.url()), { timeout: 15_000 }),
      (async () => {
        await page.getByRole('button', { name: `Delete ${FILE_NAME}`, exact: true }).click();
        await page.getByRole('button', { name: 'Delete', exact: true }).click();
      })(),
    ]);
    expect(deleteResp.status(), await deleteResp.text()).toBe(200);

    // The row is gone from the table…
    await expect(row).toBeHidden({ timeout: 15_000 });
    // …and gone from the BACKEND, not just from a client-side cache: a fresh
    // API listing must no longer contain it.
    const listing = await request.get(`/api/v2/artifacts/objects/${projectId}/${READ_BUCKET}`);
    expect(listing.status()).toBe(200);
    expect(await listing.text()).not.toContain(FILE_NAME);
  });

  /**
   * "Download selected" over a multi-select must produce a real ZIP whose
   * entries are the real objects.
   *
   * The ZIP is assembled client-side by `useZipDownload` (jszip), so the
   * archive is unzipped here and its ENTRY NAMES and CONTENT are compared
   * against the bytes this test uploaded — a stub cannot fabricate either,
   * and the archive name `<bucket>.zip` (useZipDownload.ts:42) is the
   * backend's own bucket name.
   */
  test('J20e: Download selected produces a ZIP containing the real objects', async ({ page, request }) => {
    await page.goto(BASE_URL + '/app/artifacts');
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });
    const projectId = await selectedProjectId(page);
    await seedBucketWithFiles(request, projectId);

    await page.goto(`${BASE_URL}/app/artifacts?bucket=${READ_BUCKET}`);
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });

    await expect(page.getByRole('row').filter({ hasText: FILE_NAME })).toBeVisible({ timeout: 15_000 });
    await page.getByRole('checkbox', { name: 'Select all artifacts' }).check();

    const [zip] = await Promise.all([
      page.waitForEvent('download', { timeout: 30_000 }),
      page.getByRole('button', { name: 'Download selected', exact: true }).click(),
    ]);
    expect(zip.suggestedFilename()).toBe(`${READ_BUCKET}.zip`);

    const archive = await JSZip.loadAsync(await readDownload(zip));
    expect(Object.keys(archive.files).sort()).toEqual([FILE_NAME, SECOND_FILE_NAME].sort());
    expect(await archive.file(FILE_NAME)!.async('string')).toBe(FILE_BODY);
    expect(await archive.file(SECOND_FILE_NAME)!.async('string')).toBe(SECOND_FILE_BODY);
  });

  /**
   * The artifacts upload limit must come from the SERVER's chat config, not
   * from the client's fallback constant.
   *
   * `GET /elitea_core/chat_config/prompt_lib/{projectID}` answered 404 in every
   * deployment: its only registration sat inside the never-assigned
   * `ChatService` gate on the prototype eliteacore handler, and the current
   * implementation (`promptcontextreads`) was composable only under
   * ELITEA_PROMPT_CONTEXT_READS_ENABLED → ELITEA_CONFIGURATIONS_ENABLED, which
   * no deployment sets, AND was mounted only by the production router, which
   * NewRouter never reaches. `features/artifacts`' `chatConfigApi` has been
   * querying it on every artifacts page load for the whole life of the gate,
   * silently degrading `useArtifactUpload`'s `maxFileSize` to its own 150 MB
   * `DEFAULT_MAX_FILE_SIZE` (#194).
   *
   * No journey covered the path, which is why the 404 went unnoticed. This one
   * is deliberately NOT "a request was made": `e2e-stack.sh` seeds
   * `chat_max_file_upload_size_mb = 1` into the ADMIN vault (see the seed for
   * why not project 1's), a value chosen because it differs from the reader's
   * own default, so a client that receives the config rejects a 2 MiB file and
   * a client that does not accepts it. The rejection sentence is
   * `buildArtifactUploadPlan`'s (`useArtifactUpload.ts:45`).
   *
   * Mutation-checked: adding `page.route('**\/chat_config/**', abort)` — the
   * browser-side equivalent of the 404 this endpoint used to return — makes
   * `useChatConfig` fall back to 150 MB, the 2 MiB file is accepted, and the
   * rejection assertion in step 2 fails. (Step 1 goes through the `request`
   * fixture and is unaffected by a page route, by design: the two steps fail
   * for different causes.)
   */
  test('J20f: the upload limit comes from the server chat config, not the client default', async ({ page, request }) => {
    await page.goto(BASE_URL + '/app/artifacts');
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });
    const projectId = await selectedProjectId(page);
    await seedBucketWithFiles(request, projectId);

    // 1. The route is served at all, with the seeded limits — which also
    //    exercises `lookupCurrentChatInteger`'s admin-regular fallback, since
    //    project 1's own vault is seeded empty. Every
    //    key is asserted: the response shape is the contract `readUploadLimit`
    //    reads, and all five values differ from the reader's own defaults
    //    (10/150/150/10/3), so a defaults-only body cannot satisfy this.
    const config = await request.get(`/api/v2/elitea_core/chat_config/prompt_lib/${projectId}`);
    expect(config.status(), await config.text()).toBe(200);
    expect(await config.json()).toEqual({
      chat_max_upload_count: 4,
      chat_max_upload_size_mb: 5,
      chat_max_file_upload_size_mb: 1,
      chat_max_image_upload_count: 2,
      chat_max_image_upload_size_mb: 6,
    });

    // 2. The artifacts feature ACTS on it. 2 MiB is over the seeded 1 MB limit
    //    and far under the client's 150 MB fallback, so only a client that
    //    received the config rejects this file.
    await page.goto(`${BASE_URL}/app/artifacts?bucket=${READ_BUCKET}`);
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });
    await expect(page.getByRole('row').filter({ hasText: SECOND_FILE_NAME })).toBeVisible({ timeout: 15_000 });

    const oversizedName = 'j20f-oversized-art.bin';
    await page.locator('input[type="file"]').setInputFiles({
      name: oversizedName,
      mimeType: 'application/octet-stream',
      buffer: Buffer.alloc(2 * 1024 * 1024, 7),
    });
    // The upload-path dialog is the real flow: nothing is planned until it is
    // confirmed (Artifacts.tsx:278-284).
    await page.getByRole('button', { name: 'Continue', exact: true }).click();

    await expect(page.getByRole('alert').filter({ hasText: oversizedName })).toHaveText(
      `${oversizedName}: File exceeds the upload size limit.`,
      { timeout: 15_000 },
    );
    // …and it never reached the backend.
    const afterReject = await request.get(`/api/v2/artifacts/objects/${projectId}/${READ_BUCKET}`);
    expect(afterReject.status()).toBe(200);
    expect(await afterReject.text()).not.toContain(oversizedName);

    // 3. The complement: a file UNDER the same limit is accepted and uploaded,
    //    so step 2 is a limit being enforced rather than uploading being broken.
    const acceptedName = 'j20f-small-art.bin';
    await page.locator('input[type="file"]').setInputFiles({
      name: acceptedName,
      mimeType: 'application/octet-stream',
      buffer: Buffer.alloc(512 * 1024, 3),
    });
    await page.getByRole('button', { name: 'Continue', exact: true }).click();
    await expect(page.getByRole('row').filter({ hasText: acceptedName })).toBeVisible({ timeout: 30_000 });

    const afterAccept = await request.get(`/api/v2/artifacts/objects/${projectId}/${READ_BUCKET}`);
    expect(afterAccept.status()).toBe(200);
    expect(await afterAccept.text()).toContain(acceptedName);

    // Housekeeping: this bucket is reused across runs, and J20e asserts the
    // ZIP contains EXACTLY the two seeded objects.
    const removed = await request.post(
      `/api/v2/artifacts/objects/${projectId}/${READ_BUCKET}:batchDelete`,
      { data: { keys: [acceptedName] } },
    );
    expect(removed.status(), await removed.text()).toBe(200);
  });

  /**
   * Two defects at once, both of them only reachable once the bucket is NOT
   * empty — which is why every earlier test in this file missed them.
   *
   *  1. THE ONLY UPLOAD AFFORDANCE. `ArtifactTableEmpty`'s button disappears
   *     the moment the bucket holds a file, so from the second upload on the
   *     toolbar's icon button is the whole of "upload". Every other test here
   *     calls `setInputFiles` on the hidden input directly, so none of them
   *     touches a button at all; a toolbar button that opened nothing would
   *     leave a populated bucket permanently unable to take another file, and
   *     the suite would stay green.
   *
   *  2. THE FOOTER TOTAL. The buckets panel sums `size_bytes` from the BUCKET
   *     list. The page used to answer a finished upload with
   *     `files.refetch()`, which refreshes the object list only, so the total
   *     stayed at its pre-upload value until the next full page load. The
   *     reload at the end is the anchor: what the user sees immediately has to
   *     be what the server already said.
   */
  test('J20g: the toolbar button uploads into a NON-EMPTY bucket and the size total follows', async ({ page, request }) => {
    await page.goto(BASE_URL + '/app/artifacts');
    await page.waitForURL('**/artifacts**', { timeout: 15_000 });
    const projectId = await selectedProjectId(page);
    await seedBucketWithFiles(request, projectId);

    await page.goto(`${BASE_URL}/app/artifacts?bucket=${READ_BUCKET}`);
    await expect(page.getByRole('row').filter({ hasText: FILE_NAME })).toBeVisible({ timeout: 15_000 });
    // The precondition the whole test rests on: the empty state, and with it
    // its own upload button, is gone.
    await expect(page.getByText('No files in this bucket')).toHaveCount(0);

    const footerSize = page.getByText('Size:', { exact: true }).locator('xpath=following-sibling::*[1]');
    const sizeBefore = (await footerSize.textContent())?.trim();
    expect(sizeBefore, 'the footer must report a total before the upload').toBeTruthy();

    // Clicking the button must OPEN the picker. `waitForFileChooser` fails the
    // test if it does not, which `setInputFiles` on the hidden input never
    // could.
    const extraName = 'j20g-toolbar-art.bin';
    const [chooser] = await Promise.all([
      page.waitForEvent('filechooser', { timeout: 15_000 }),
      page.getByRole('button', { name: 'Upload files' }).click(),
    ]);
    await chooser.setFiles({
      name: extraName,
      mimeType: 'application/octet-stream',
      // 400 KiB: large enough that the project total's rendered string cannot
      // stay the same, whatever the other buckets already hold.
      buffer: Buffer.alloc(400 * 1024, 5),
    });
    await page.getByRole('button', { name: 'Continue', exact: true }).click();

    await expect(page.getByRole('row').filter({ hasText: extraName })).toBeVisible({ timeout: 30_000 });

    // No reload between here and the upload.
    await expect.poll(async () => (await footerSize.textContent())?.trim(), { timeout: 20_000 }).not.toBe(sizeBefore);
    const sizeAfter = (await footerSize.textContent())?.trim();

    // …and the value shown was the RIGHT one, not merely a different one.
    await page.reload();
    await expect(page.getByRole('row').filter({ hasText: extraName })).toBeVisible({ timeout: 30_000 });
    await expect(footerSize).toHaveText(sizeAfter as string);

    // Housekeeping: J20e asserts the ZIP holds EXACTLY the two seeded objects.
    const removed = await request.post(
      `/api/v2/artifacts/objects/${projectId}/${READ_BUCKET}:batchDelete`,
      { data: { keys: [extraName] } },
    );
    expect(removed.status(), await removed.text()).toBe(200);
  });
});
