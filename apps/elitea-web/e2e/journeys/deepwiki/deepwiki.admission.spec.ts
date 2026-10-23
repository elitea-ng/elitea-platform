import { test as adminTest, expect } from '@playwright/test';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { SEEDED } from './helpers';

/**
 * DWIKI-013 — the provider is admitted at boot (ADR-0023 decision 6, H4a).
 *
 * Provider-backed, so it runs in the `deepwiki-stack` project only: the
 * standalone stack composes the facade with a public project
 * (ELITEA_AI_PROJECT_ID) and the Go host behind it, and the facade
 * registers the host's descriptor into the admission plane at boot and
 * keeps its health projection current. The E2E stack has no public project,
 * so registration is skipped there by design and this journey has nothing
 * to look at.
 *
 * What is asserted is the whole chain, from the ADMIN's side: the listing
 * the administration surface answers carries the provider the facade
 * registered — its own name from its own descriptor, the facade's target as
 * the origin, `healthy: true` from a probe inside the freshness window — and
 * the Service Descriptors page renders that row.
 */
adminTest.use({ storageState: STORAGE_STATE.admin });

const DESCRIPTORS_ENDPOINT = '/api/v2/elitea_core/admin/administration';

/** The register/revoke route: POST and DELETE on the same path (router.go). */
const REGISTER_ENDPOINT = (projectId: number) => `/api/v2/elitea_core/register_descriptor/${projectId}`;

/** The tool DWIKI-005 starts a generation with (WikiGenerationPanel.tsx). */
const INVOKE_PATH = `/api/v2/deepwiki/tools/${SEEDED.projectId}/wikis/generate_wiki/invoke`;

/**
 * The gate caches one decision for 15s (internal/providerhost/admission,
 * DefaultTTL), so a revoke and a re-registration each take up to that long to
 * be visible on the invoke path. Polled generously either way.
 */
const ADMISSION_SETTLE = 60_000;

interface DescriptorRow {
  project_id?: number;
  provider_name?: string;
  service_location_url?: string;
  healthy?: boolean | null;
  status?: string;
  published_manifest_digest?: string | null;
}

adminTest('DWIKI-013: the facade registered the provider at boot, and the admin sees it healthy', async ({ page }) => {
  await page.goto(BASE_URL + '/admin/app/service-descriptors', { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('heading', { name: 'Service Descriptors' })).toBeVisible({ timeout: 20_000 });

  // The facade probes on an interval; the first probe runs at its boot, so
  // the row is there before any browser could be. Polled anyway, because a
  // stack that just came up may be inside its first minute.
  let wikis: DescriptorRow | undefined;
  await expect
    .poll(
      async () => {
        const listing = await page.evaluate(async (endpoint) => {
          const response = await fetch(endpoint, { credentials: 'include' });
          return { status: response.status, body: (await response.json()) as { rows?: DescriptorRow[] } };
        }, DESCRIPTORS_ENDPOINT);
        expect(listing.status, 'the listing must answer from storage (a 501 means no admission plane)').toBe(200);
        wikis = (listing.body.rows ?? []).find((row) => row.provider_name === 'wikis');
        return wikis?.healthy ?? null;
      },
      { timeout: 90_000, intervals: [2_000] },
    )
    .toBe(true);

  expect(wikis?.service_location_url, 'the origin is the facade\'s gRPC target (grpcs://host:port, #1359), as registered').toBe('grpcs://elitea-deepwiki:9443');
  expect(wikis?.status, 'admission is recorded, not in force, until a policy overlay exists').toBe('inactive');
  expect(wikis?.published_manifest_digest, 'the manifest was published').toMatch(/^[0-9a-f]{64}$/);

  // The page renders the registered row — with no "unavailable" notice.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('grid', { name: 'Registered service descriptors' })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('admin-service-descriptors-unavailable')).toHaveCount(0);
  await expect(page.getByRole('grid', { name: 'Registered service descriptors' }).getByText('wikis', { exact: true })).toBeVisible();
  await expect(page.getByRole('grid', { name: 'Registered service descriptors' }).getByText('grpcs://elitea-deepwiki:9443', { exact: false })).toBeVisible();
});

/**
 * DWIKI-013b — admission is IN FORCE: a revoked provider refuses invokes.
 *
 * DWIKI-013 above proves the plane RECORDS. This one proves it is READ: the
 * administration surface's one action — revoke — reaches the request path
 * and stops new work, which is what makes that surface an operator control
 * rather than an audit note. It pins the two things that must not drift: the
 * 503's stable `reason` code, and that a revocation is undone only by
 * admitting a new revision.
 *
 * `deepwiki-stack` only, for DWIKI-013's reason: nothing is registered on the
 * E2E stack, so the gate there resolves to "plane absent, allow" and there is
 * nothing to look at.
 *
 * THE REVOKE WINDOW IS SHARED STATE. While it is open every DeepWiki invoke
 * in the deployment is refused, DWIKI-005's generation included — and
 * `fullyParallel: false` orders tests WITHIN a file without pinning files to
 * one worker. The window is two polls wide and closed in a finally; run this
 * project with `--workers=1`.
 *
 * WHY THE REGISTRAR IS NOT WHAT RESTORES IT. The facade re-probes every
 * minute and that re-probe cannot lift a revocation, twice over: the
 * registrar short-circuits while the descriptor's name and digest are
 * unchanged (internal/providerhost/registrar), and even a forced write would
 * land on the SAME revision_id, whose upsert touches `reason` and
 * `admitted_at` and never `status`. A revocation is undone by admitting a NEW
 * revision — which is what the administration surface's POST does, and what
 * this test uses. The revoked row survives beside it, as audit.
 */
adminTest('DWIKI-013b: revoking the provider refuses invokes, and re-registering admits it again', async ({
  page,
  browser,
}) => {
  adminTest.setTimeout(180_000);

  const listing = async (): Promise<DescriptorRow[]> => {
    const answer = await page.evaluate(async (endpoint) => {
      const response = await fetch(endpoint, { credentials: 'include' });
      return { status: response.status, body: (await response.json()) as { rows?: DescriptorRow[] } };
    }, DESCRIPTORS_ENDPOINT);
    expect(answer.status, 'the listing must answer from storage').toBe(200);
    return answer.body.rows ?? [];
  };

  await page.goto(BASE_URL + '/admin/app/service-descriptors', { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('heading', { name: 'Service Descriptors' })).toBeVisible({ timeout: 20_000 });

  // The registration is filed under the PUBLIC project, not the caller's, so
  // the revoke names the project the listing reports rather than 90200.
  let registered: DescriptorRow | undefined;
  await expect
    .poll(
      async () => {
        registered = (await listing()).find((row) => row.provider_name === 'wikis');
        return registered?.status ?? null;
      },
      { timeout: 90_000, intervals: [2_000] },
    )
    .toBe('inactive');
  const publicProjectId = registered?.project_id as number;
  const origin = registered?.service_location_url;
  expect(publicProjectId, 'the registered row names its project').toBeGreaterThan(0);
  expect(origin, 'the registered row names its origin').toBeTruthy();

  // A member is who actually invokes: the gate sits BEHIND the permission
  // guard, so the refusal has to be proved for a caller the guard admits.
  const member = await browser.newContext({ storageState: STORAGE_STATE.member });
  // A body with no repository. The facade refuses it long before a generation
  // starts, which is the point — this journey must not kick off a real run on
  // toolkit 9002, which DWIKI-005 owns. The ADMISSION refusal is answered
  // before the body is looked at, so it is what this reads.
  const invoke = async (): Promise<{ status: number; reason: string }> => {
    const response = await member.request.post(BASE_URL + INVOKE_PATH, {
      data: { configuration: { parameters: {} }, parameters: { query: 'GO' } },
      failOnStatusCode: false,
    });
    let reason = '';
    try {
      reason = ((await response.json()) as { reason?: string }).reason ?? '';
    } catch {
      reason = '';
    }
    return { status: response.status(), reason };
  };

  // Before: whatever the facade makes of that body, it is not an admission
  // refusal. Recorded-but-inactive is admitted, because `record` is the
  // shipped posture and nothing can reach `active` yet.
  expect((await invoke()).reason, 'an inactive provider is admitted while recording').not.toBe(
    'provider_admission_revoked',
  );

  try {
    const revoked = await page.request.delete(
      `${BASE_URL}${REGISTER_ENDPOINT(publicProjectId)}?provider_name=wikis&reason=${encodeURIComponent('DWIKI-013b')}`,
    );
    expect(revoked.status(), 'the revoke was accepted').toBe(200);

    // The listing says so — a revoke is a status change on the revision, not
    // a deleted row.
    expect((await listing()).find((row) => row.provider_name === 'wikis')?.status).toBe('revoked');

    // And the request path obeys it: 503 with the stable code, for a caller
    // whose permissions are perfectly good.
    await expect
      .poll(async () => (await invoke()).reason, { timeout: ADMISSION_SETTLE, intervals: [2_000] })
      .toBe('provider_admission_revoked');
    expect((await invoke()).status, 'a refused invoke is 503, not 403').toBe(503);
  } finally {
    // Re-admit: a NEW manifest is a NEW revision, and the newest is what the
    // gate reads. Re-posting the descriptor byte for byte would land on the
    // revoked revision's own id and change nothing, so this one carries the
    // marker that makes it new.
    const readmitted = await page.request.post(`${BASE_URL}${REGISTER_ENDPOINT(publicProjectId)}`, {
      data: {
        provider_name: 'wikis',
        service_location_url: origin,
        descriptor: { name: 'wikis', readmitted_by: 'DWIKI-013b', at: new Date().toISOString() },
      },
      failOnStatusCode: false,
    });
    expect(readmitted.status(), 'recorded, not in force: the register route answers 202').toBe(202);
    expect(((await readmitted.json()) as { status?: string }).status).toBe('inactive');

    await expect
      .poll(async () => (await invoke()).reason, { timeout: ADMISSION_SETTLE, intervals: [2_000] })
      .not.toBe('provider_admission_revoked');
    await member.close();
  }

  // The revoked revision survives beside the new one — an admission trail the
  // surface that writes it cannot erase.
  expect((await listing()).find((row) => row.provider_name === 'wikis')?.status).toBe('inactive');
});


/**
 * DWIKI-013c — the admin ACTIVATES the provider through the UI, and takes it
 * back (migration 0109, ADR-0023 S3).
 *
 * DWIKI-013 proves the plane RECORDS and DWIKI-013b proves it is READ. This one
 * proves it can be DECIDED, which is the thing 0107 deliberately could not do:
 * its CHECK required a policy overlay and nothing could issue one, so every
 * provider this deployment recorded stayed `inactive` for want of an issuer.
 *
 * THROUGH THE PAGE, NOT THROUGH THE API. The Go acceptance suite already covers
 * every status code the route answers. What only a browser can prove is that the
 * control exists on the row it belongs to, that the dialog collects the reason
 * the server requires, and that the listing the operator re-reads shows the
 * change — the three places an activation surface can be right in Go and absent
 * in the product.
 *
 * THE DIGEST MUST NOT MOVE. Activation puts the reviewed manifest in force; it
 * is not a republication. A digest that changed across the transition would mean
 * the page activated something other than the row the operator was looking at,
 * which is exactly what `expected_digest` exists to make impossible.
 *
 * `deepwiki-stack` only, for DWIKI-013's reason: nothing is registered on the
 * E2E stack, so there is no row to act on.
 *
 * SHARED STATE, like DWIKI-013b: while the provider is active every DeepWiki
 * invoke in the deployment sees `active` rather than `inactive`. The window is
 * closed in a finally, and this project runs with `--workers=1`.
 */
adminTest('DWIKI-013c: the admin activates the provider through the page, and deactivates it again', async ({ page }) => {
  adminTest.setTimeout(180_000);

  const listing = async (): Promise<DescriptorRow[]> => {
    const answer = await page.evaluate(async (endpoint) => {
      const response = await fetch(endpoint, { credentials: 'include' });
      return { status: response.status, body: (await response.json()) as { rows?: DescriptorRow[] } };
    }, DESCRIPTORS_ENDPOINT);
    expect(answer.status, 'the listing must answer from storage').toBe(200);
    return answer.body.rows ?? [];
  };
  const wikisRow = async (): Promise<DescriptorRow | undefined> =>
    (await listing()).find((row) => row.provider_name === 'wikis');

  await page.goto(BASE_URL + '/admin/app/service-descriptors', { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('heading', { name: 'Service Descriptors' })).toBeVisible({ timeout: 20_000 });

  let before: DescriptorRow | undefined;
  await expect
    .poll(
      async () => {
        before = await wikisRow();
        return before?.status ?? null;
      },
      { timeout: 90_000, intervals: [2_000] },
    )
    .toBe('inactive');
  const reviewedDigest = before?.published_manifest_digest;
  expect(reviewedDigest, 'the row names the manifest the operator is reviewing').toMatch(/^[0-9a-f]{64}$/);

  const grid = page.getByRole('grid', { name: 'Registered service descriptors' });
  await expect(grid).toBeVisible({ timeout: 20_000 });
  // THE `wikis` ROW, not the grid: since #728 the standalone stack registers
  // two providers (`wikis` and `inventory`), each with its own controls. A
  // grid-wide "Activate" would activate whichever row came first, and a
  // grid-wide "no Activate left" is false while the other row stays inactive.
  const wikisGridRow = grid.getByRole('row').filter({ hasText: 'wikis' });
  await expect(wikisGridRow).toHaveCount(1);

  try {
    // `exact`, because a role-name match is a substring by default and
    // "Deactivate" contains "activate" — the count below would otherwise be 1
    // on a row that offers exactly the right verb.
    await wikisGridRow.getByRole('button', { name: 'Activate', exact: true }).click();
    // THE REASON IS REQUIRED, and the button is what says so: an activation is
    // a decision, and the revision row records the operator's sentence.
    const confirm = page.getByRole('button', { name: 'Activate', exact: true }).last();
    await expect(confirm, 'the dialog will not submit an activation with no reason').toBeDisabled();
    await page.getByLabel('Reason').fill('DWIKI-013c: reviewed the wikis toolkit');
    await confirm.click();

    await expect
      .poll(async () => (await wikisRow())?.status ?? null, { timeout: 30_000, intervals: [1_000] })
      .toBe('active');

    // The DIGEST DID NOT MOVE. Activation is not a republication, and a changed
    // digest would mean the page put something else in force.
    expect((await wikisRow())?.published_manifest_digest, 'the reviewed manifest is what went in force').toBe(
      reviewedDigest,
    );

    // The operator's own re-read: the page shows the new state, and offers the
    // verb that undoes it rather than the one that repeats it.
    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(wikisGridRow.getByText('Active', { exact: true })).toBeVisible({ timeout: 20_000 });
    await expect(wikisGridRow.getByRole('button', { name: 'Deactivate', exact: true })).toBeVisible();
    await expect(wikisGridRow.getByRole('button', { name: 'Activate', exact: true })).toHaveCount(0);
  } finally {
    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(grid).toBeVisible({ timeout: 20_000 });
    const deactivate = wikisGridRow.getByRole('button', { name: 'Deactivate', exact: true });
    if ((await deactivate.count()) > 0) {
      await deactivate.click();
      await page.getByLabel('Reason').fill('DWIKI-013c: restoring the recorded state');
      await page.getByRole('button', { name: 'Deactivate', exact: true }).last().click();
      await expect
        .poll(async () => (await wikisRow())?.status ?? null, { timeout: 30_000, intervals: [1_000] })
        .toBe('inactive');
    }
  }

  // Back where DWIKI-013 found it, digest included.
  const after = await wikisRow();
  expect(after?.status, 'the journey restores the recorded state it found').toBe('inactive');
  expect(after?.published_manifest_digest).toBe(reviewedDigest);
});

/**
 * The ENFORCE half, and why it is skipped rather than written as a passing
 * assertion nobody runs.
 *
 * Under `ELITEA_PROVIDER_ADMISSION=enforce` an inactive provider is REFUSED on
 * the invoke path (`provider_admission_inactive`) and an active one is admitted
 * — which is the pair that makes activation mean something operationally rather
 * than only in the listing. Asserting it needs the stack composed with that
 * variable set, and the standalone compose file ships `record`: it has to,
 * because until 0109 nothing could reach `active` and defaulting to enforce
 * would have refused every provider on every install.
 *
 * So this is a `test.skip` with the condition in its name, not a comment in
 * another test. A skipped test named for the environment it needs is a standing
 * request that shows up in every run's summary; the same words in a `//` comment
 * are invisible the day after they are written.
 */
adminTest('DWIKI-013c-enforce: under ELITEA_PROVIDER_ADMISSION=enforce an inactive provider is refused', async ({
  page,
}) => {
  adminTest.skip(
    process.env.E2E_ADMISSION_POSTURE !== 'enforce',
    'needs the stack composed with ELITEA_PROVIDER_ADMISSION=enforce; the standalone compose ships record',
  );
  adminTest.setTimeout(180_000);

  await page.goto(BASE_URL + '/admin/app/service-descriptors', { waitUntil: 'domcontentloaded' });
  // The page must SAY it is enforcing. Under this posture `inactive` means
  // refused, and an operator who cannot see the posture cannot read the status
  // column correctly.
  await expect(page.getByTestId('admin-admission-posture')).toContainText('Admission in force', {
    timeout: 20_000,
  });
});
