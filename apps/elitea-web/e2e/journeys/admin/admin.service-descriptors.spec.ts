/**
 * Journey 35: Admin › Service Descriptors lists what is registered, and its
 *             registration verbs record rather than pretend (JRNY-035)
 *
 * ## What this journey used to assert, and why it changed
 *
 * Until migration 0107 all three endpoints answered 501, and this journey's job
 * was to prove the refusal was the SERVER's and not a sentence the page carried.
 * That mattered: before unit A14 the listing answered 200 with three hardcoded
 * rows — `elitea_core`, `auth` and `indexer`, all "active" at version "2.0.0",
 * which are Pylon plugin names and not providers — and the registration verbs
 * had a dead handler answering `{"ok": true}` to a body it discarded. An
 * operator reading either would have concluded something false.
 *
 * The admission plane replaced the refusals with storage, so asserting 501 now
 * asserts the absence of the feature. The assertions below are aimed at the
 * same failure the old ones were, one contract later:
 *
 *  - the rows on screen are the rows the ENDPOINT returned, checked by fetching
 *    the endpoint from the page and comparing;
 *  - none of the three invented rows is among them;
 *  - a registration is RECORDED and reported `inactive` with a reason — never
 *    `{"ok": true}`, and never `active`, because admitting one needs a policy
 *    overlay this deployment cannot issue and a CHECK constraint enforces that;
 *  - a revoke that matches nothing answers 404 rather than reporting success;
 *  - the Configuration section points at this page instead of repeating a
 *    refusal the page no longer makes.
 *
 * ## Per-engine partitioning: NOW REQUIRED
 *
 * The header here used to say none was needed, because "every test here is a
 * read or a refused write". That is no longer true — J35d's POST is STORED. Two
 * engines forging the same provider name against one stack would race on the
 * partial unique index over `(project_id, provider_id)`, and the loser would
 * see a failure that looks like a defect in the handler.
 *
 * So the forged provider name carries the engine's own name, and each engine
 * revokes what it registered. Everything else is still a read and still safe to
 * run concurrently.
 */
import { test as adminTest, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

const DESCRIPTORS_ENDPOINT = '/api/v2/elitea_core/admin/administration';
const REGISTER_ENDPOINT = '/api/v2/elitea_core/register_descriptor/1';

async function openServiceDescriptors(page: Page): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/service-descriptors', {
    waitUntil: 'domcontentloaded',
  });
  expect(
    response?.status(),
    'the admin SPA must serve the service-descriptors route, not 404',
  ).toBeLessThan(400);
  await expect(page.getByRole('heading', { name: 'Service Descriptors' })).toBeVisible({
    timeout: 20_000,
  });
}

adminTest('J35: the page renders the listing the server returned', async ({ page }) => {
  await openServiceDescriptors(page);

  // The GRID, which this page renders only on success. During load it shows its
  // title and a progress bar and nothing else, so the grid's presence is the
  // statement that the query resolved.
  await expect(page.getByRole('grid', { name: 'Registered service descriptors' })).toBeVisible({
    timeout: 20_000,
  });

  // The refusal is GONE. Its presence would mean the admission plane is absent
  // on this stack — no database, or migration 0107 unapplied — which is a
  // deployment fault and not an empty listing.
  await expect(page.getByTestId('admin-service-descriptors-unavailable')).toHaveCount(0);

  // A load error would mean the page could not reach the endpoint at all, and a
  // 403 would mean the gate and the seed had drifted apart. Either would leave
  // an operator with no explanation.
  await expect(page.getByTestId('admin-service-descriptors-error')).toHaveCount(0);

  await checkA11y(page);
});

adminTest('J35b: the rows on screen are the rows the endpoint returned', async ({ page }) => {
  await openServiceDescriptors(page);

  const listing = await page.evaluate(async (endpoint) => {
    const response = await fetch(endpoint, { credentials: 'include' });
    return {
      status: response.status,
      body: (await response.json()) as {
        rows?: { provider_name?: string; healthy?: boolean | null }[];
        total?: number;
        error?: string;
      },
    };
  }, DESCRIPTORS_ENDPOINT);

  // 200 with a listing, not 501 and not 403. With the permission held, this is
  // the deployment's own answer about its own state.
  expect(
    listing.status,
    'the listing must answer from storage; a 501 means the admission plane is absent on this stack',
  ).toBe(200);
  expect(Array.isArray(listing.body.rows), 'the body must carry a rows array').toBe(true);
  expect(listing.body.total).toBe(listing.body.rows?.length);

  const rows = listing.body.rows ?? [];

  // Every provider the endpoint named is on the page, as many times as the
  // endpoint named it. A page rendering a subset would be the #132 shape: a
  // 200 in the network tab and less on screen than the server sent.
  //
  // MATCHED ON THE PROVIDER CELL, not on page text, and this is the whole
  // point of the rewrite. The assertion used to be
  // `page.getByText(name, { exact: false }).toHaveCount(1)`, which is a
  // SUBSTRING match over the whole document — and a provider's own name is a
  // substring of the origin the next column shows: `inventory` occurs in both
  // the Provider cell and `https://elitea-inventory:8080`. The count was 2 for
  // one correct row, so this journey failed on a page that was rendering
  // exactly what the endpoint returned. It only ever passed because the stack
  // registered nothing and the loop had no rows to run on; the seed now
  // restarts elitea-main after writing its project rows, so the facades'
  // boot-time registrars reach the admission plane and the listing is no
  // longer empty.
  //
  // A COUNT PER NAME, not a flat 1: `(project_id, provider_id)` is the
  // registration key, so one provider name may legitimately appear once per
  // project. Comparing the multiset keeps the "less on screen than the server
  // sent" failure detectable in that case too.
  const expected = new Map<string, number>();
  for (const row of rows) {
    const name = row.provider_name ?? '__unnamed__';
    expected.set(name, (expected.get(name) ?? 0) + 1);
  }
  for (const [name, count] of expected) {
    await expect(page.getByRole('gridcell', { name, exact: true })).toHaveCount(count);
  }

  // `healthy` is THREE-STATE and the column must be able to say so. A row the
  // server reports as null is one nobody has probed lately, which is a different
  // fact from "down" — the one the projection table exists to keep separable.
  for (const row of rows) {
    expect(
      [true, false, null, undefined],
      'healthy must be a three-state value, not a string or a number',
    ).toContain(row.healthy ?? null);
  }
});

adminTest('J35c: none of the invented rows is rendered, and no row offers a delete', async ({
  page,
}) => {
  await openServiceDescriptors(page);
  await expect(page.getByRole('grid', { name: 'Registered service descriptors' })).toBeVisible({
    timeout: 20_000,
  });

  // The three rows the replaced handler invented, by the names it invented.
  // They are Pylon PLUGIN names; a provider hub holding them would mean the
  // stub came back.
  for (const invented of ['2.0.0', 'Core platform service', 'Authentication service']) {
    await expect(page.getByText(invented, { exact: false })).toHaveCount(0);
  }

  // The reference page puts a delete icon on every row behind a
  // `window.confirm`. Revoking is a real verb now, but this page issues no
  // control for it: a revoke needs a reason and an actor, and a confirm dialog
  // collects neither.
  await expect(page.getByRole('button', { name: /delete/i })).toHaveCount(0);
});

adminTest('J35d: a registration is recorded as inactive, with a reason', async ({
  page,
}, testInfo) => {
  // Named per engine. This test WRITES, and two engines forging one name would
  // race on the partial unique index over (project_id, provider_id).
  const provider = `e2e-forged-provider-${testInfo.project.name}`;

  await openServiceDescriptors(page);

  // Forged, not clicked — no control on the page issues these, which is the
  // point. What must not happen is a registration that reports success and
  // stores nothing: the operator points a provider at Elitea, sees it accepted,
  // and finds out it was never reachable when an agent fails to call its tools.
  const registered = await page.evaluate(
    async ({ endpoint, name }) => {
      const post = await fetch(endpoint, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        // The ADMISSION PLANE's shape, not pylon's. `provider_name`, an ORIGIN
        // with no path, and the descriptor itself — see the rejection test
        // below for the shape this replaces and why sending it is a 400.
        body: JSON.stringify({
          provider_name: name,
          service_location_url: 'https://provider.example.invalid',
          descriptor: {
            provider_name: name,
            version: '1.0.0',
            toolkits: [],
          },
        }),
      });
      return { status: post.status, body: await post.text() };
    },
    { endpoint: REGISTER_ENDPOINT, name: provider },
  );

  // 202, not 200. 200 would say the provider is admitted, and admitting one
  // needs a policy overlay this deployment cannot issue.
  expect(registered.status, 'a registration is recorded, not put in force').toBe(202);
  const body = JSON.parse(registered.body) as {
    status?: string;
    reason?: string;
    admitted_provider_revision?: unknown;
    published_manifest_digest?: unknown;
    ok?: unknown;
  };
  expect(body.status, 'a recorded descriptor is inactive until an overlay admits it').toBe(
    'inactive',
  );
  expect(body.reason, 'the response must say WHY it is inactive').toBeTruthy();
  expect(body.admitted_provider_revision, 'the response names the revision it created').toBeTruthy();
  expect(body.published_manifest_digest, 'the response cites the manifest digest').toBeTruthy();
  // Not `{"ok": true}` — the exact body the dead handler returned.
  expect(body.ok).toBeUndefined();

  // It LANDED: a reload shows it in the listing, which is the half a 202 alone
  // cannot prove.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await openServiceDescriptors(page);
  const recordedRow = page.getByRole('row').filter({ hasText: provider });
  await expect(recordedRow).toHaveCount(1);
  // INACTIVE on the screen too, not merely in the 202 body: a page that showed
  // it as live would be the lie the 202 exists to avoid.
  await expect(recordedRow.getByText('Inactive')).toBeVisible({ timeout: 15_000 });

  // REVOKING DOES NOT REMOVE THE ROW, and this assertion is the corrected one.
  //
  // The first version expected the provider to disappear. It does not: the
  // listing is driven by `provider_origin_registration` — every origin ever
  // registered — and DELETE sets the latest revision's status to `revoked`
  // rather than deleting anything, because an admission that was once in force
  // is a fact about what this deployment ran.
  //
  // Which makes the ADMISSION COLUMN the thing to assert. Without it a revoked
  // provider sits in the table looking exactly like a live one, and an operator
  // who revokes it, reloads, and sees no change concludes the revoke failed.
  // That column did not exist until this journey's first version failed here.
  const revoked = await page.evaluate(
    async ({ endpoint, name }) => {
      const response = await fetch(
        `${endpoint}?provider_name=${encodeURIComponent(name)}&reason=e2e+cleanup`,
        { method: 'DELETE', credentials: 'include' },
      );
      return { status: response.status, body: await response.text() };
    },
    { endpoint: REGISTER_ENDPOINT, name: provider },
  );
  expect(revoked.status, 'revoking a registered provider must succeed').toBe(200);

  await page.reload({ waitUntil: 'domcontentloaded' });
  await openServiceDescriptors(page);
  const revokedRow = page.getByRole('row').filter({ hasText: provider });
  await expect(revokedRow).toHaveCount(1);
  await expect(revokedRow.getByText('Revoked')).toBeVisible({ timeout: 15_000 });

  // And the SERVER agrees, which is what says the column is reading storage
  // rather than remembering the click that produced it.
  const afterRevoke = await page.evaluate(
    async ({ endpoint, name }) => {
      const response = await fetch(endpoint, { credentials: 'include' });
      const body = (await response.json()) as { rows?: { provider_name?: string; status?: string }[] };
      return (body.rows ?? []).find((row) => row.provider_name === name)?.status ?? null;
    },
    { endpoint: DESCRIPTORS_ENDPOINT, name: provider },
  );
  expect(afterRevoke).toBe('revoked');
});

adminTest('J35d1: a registration missing what admission needs is refused, not stored', async ({
  page,
}) => {
  // The BODY IS PART OF THE CONTRACT, and this is the shape pylon accepted:
  // `name`, a URL with a path, `configuration` and `provided_toolkits`, and no
  // descriptor. Recording it would create a revision citing a manifest that
  // does not exist, so it is refused with a message about the input rather
  // than a 500 carrying a constraint name.
  await openServiceDescriptors(page);

  const refusals = await page.evaluate(async (endpoint) => {
    const send = async (body: unknown) => {
      const response = await fetch(endpoint, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      return { status: response.status, body: await response.text() };
    };
    return {
      pylonShaped: await send({
        name: 'e2e-pylon-shaped',
        service_location_url: 'https://provider.example.invalid/e2e',
        configuration: {},
        provided_toolkits: [],
      }),
      pathInOrigin: await send({
        provider_name: 'e2e-path-in-origin',
        service_location_url: 'https://provider.example.invalid/with/a/path',
        descriptor: { provider_name: 'e2e-path-in-origin' },
      }),
      noDescriptor: await send({
        provider_name: 'e2e-no-descriptor',
        service_location_url: 'https://provider.example.invalid',
      }),
    };
  }, REGISTER_ENDPOINT);

  for (const [label, result] of Object.entries(refusals)) {
    expect(result.status, `${label} must be refused as a bad request`).toBe(400);
    // The message is about the INPUT. A 500 with a constraint name would send
    // the operator looking at the database instead of at their request.
    expect(JSON.parse(result.body) as { error?: string }).toHaveProperty('error');
  }

  // And none of them landed.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await openServiceDescriptors(page);
  for (const name of ['e2e-pylon-shaped', 'e2e-path-in-origin', 'e2e-no-descriptor']) {
    await expect(page.getByText(name, { exact: false })).toHaveCount(0);
  }
});

adminTest('J35d2: a revoke that matches nothing is reported, not swallowed', async ({ page }) => {
  await openServiceDescriptors(page);

  // A revoke that matched nothing is usually a misspelt provider. Reporting it
  // as done sends the operator away believing they turned something off.
  const missing = await page.evaluate(async (endpoint) => {
    const response = await fetch(`${endpoint}?provider_name=e2e-never-registered`, {
      method: 'DELETE',
      credentials: 'include',
    });
    return response.status;
  }, REGISTER_ENDPOINT);
  expect(missing).toBe(404);

  // And a revoke that names nothing at all is a bad request, not a wildcard.
  const unnamed = await page.evaluate(async (endpoint) => {
    const response = await fetch(endpoint, { method: 'DELETE', credentials: 'include' });
    return response.status;
  }, REGISTER_ENDPOINT);
  expect(unnamed, 'an unnamed revoke must not be read as "revoke everything"').toBe(400);
});

adminTest('J35e: the Configuration section points at the page, not at a refusal', async ({
  page,
}) => {
  // An operator can reach this subject from two places. In the reference SPA the
  // reachable one is actually the Configuration section — its standalone page
  // has no route and nothing imports it.
  //
  // The two used to state the SAME sentence, because both refused. Now the page
  // answers, so a section still carrying the refusal would tell an operator this
  // deployment has no descriptor store while the page it points at lists one.
  await page.goto(BASE_URL + '/admin/app/configuration', { waitUntil: 'domcontentloaded' });
  // Waits for the SECTION THIS TEST CLICKS, not for an unrelated one.
  //
  // This was `/Resources/` — a landmark that meant "the sidebar has
  // rendered" and happened to be a different section entirely. Unit A14's
  // Features page moved `resources` off Configuration (the reference puts it
  // there, and #217 recorded that it should), and this assertion went with
  // it. Waiting on the thing the next line clicks cannot drift that way
  // again.
  await expect(page.getByRole('button', { name: /Service Descriptors/ })).toBeVisible({
    timeout: 20_000,
  });

  await page.getByRole('button', { name: /Service Descriptors/ }).click();

  const notice = page.getByTestId('admin-configuration-unavailable');
  await expect(notice).toBeVisible();
  // It NAMES the page, so the pointer is followable.
  await expect(notice).toContainText('/admin/app/service-descriptors');
  // And it does not claim the platform has no provider hub — the words the
  // endpoints' own refusal uses, which is still the right answer when the
  // admission plane is absent and the wrong one here.
  await expect(notice).not.toContainText(/no descriptor store/i);
});
