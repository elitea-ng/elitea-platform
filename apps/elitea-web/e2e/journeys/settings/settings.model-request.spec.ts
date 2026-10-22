/**
 * Settings › AI Configuration: "Request a model connection" files a real
 * moderation row against a real project, as the signed-in member.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS ADDS OVER `RequestModelConnection.test.tsx`
 * ─────────────────────────────────────────────────────────────────────────────
 * That unit test pins the WIRE exhaustively — the route, the `issue_type`, the
 * two-field body, the `provider:`/`model:` prefixes, the slashed-model-name
 * encoding, the per-field validation — against `msw`. Everything it asserts is
 * about what the browser SENDS.
 *
 * Three things it cannot say, and this journey does:
 *
 *  1. The control is REACHABLE. It renders on `/app/settings/model-configuration`
 *     beside the copy-configuration button, on a page that also has to have
 *     loaded seven per-section configuration queries before it mounts anything.
 *  2. The POST is AUTHORISED. `POST /admin/moderation_status/default/{p}/{e}`
 *     resolves `admin.moderation.create` against the caller's membership of the
 *     project; a mocked 201 proves nothing about that, and the button is
 *     deliberately not permission-gated in the UI, so a missing grant surfaces
 *     here and nowhere else.
 *  3. The row EXISTS afterwards. The read-back is the caller's own
 *     `GET /admin/moderation_status/default/{p}/{entity}`, which is scoped to
 *     the requester in SQL — the same call the App Catalogue's "Request Access"
 *     card reads, and the one whose legacy predecessor answered
 *     `{"status":"approved"}` to every caller for every entity. A POST that
 *     answered 201 and wrote nothing passes the unit test and fails this.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * A moderation row cannot be deleted — the surface has a POST, a GET and a
 * decision PUT, and no DELETE at any mode (issue #544). So the row this file
 * files is DECIDED on the way out, by the admin persona, so the operator's
 * Pending tab ends the run as it started; `scripts/e2e-stack.sh seed` is the
 * only step with a database to remove the rows themselves.
 */
import { test, expect, request as apiRequest } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, DEFAULT_PROJECT_ID } from '../../fixtures/api';

/** The label that separates this from the app-access queue. It is the queue's only discriminator between the two. */
const ISSUE_TYPE = 'Model Connection Request';

/** Entities this run filed, so the teardown can decide them. Filled only on a 201. */
const filedEntities: string[] = [];

test.afterAll(async () => {
  if (filedEntities.length === 0) return;
  // The MEMBER cannot decide their own request — that is refused server-side —
  // so the teardown signs in as the operator, exactly as J34's does.
  const api = await apiRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.admin });
  try {
    for (const entity of filedEntities) {
      const queue = await api.get(
        `${API_BASE}/admin/moderation_statuses/administration` +
          `?entity_id=${encodeURIComponent(entity)}&sort_by=created_at&sort_order=desc&limit=100&offset=0`,
      );
      if (!queue.ok()) {
        // eslint-disable-next-line no-console -- a silent teardown is how the Pending tab grows a row per run
        console.warn(`model-request teardown: cannot read ${entity} (${String(queue.status())})`);
        continue;
      }
      const body = (await queue.json()) as { rows?: { id: number; status: string }[] };
      for (const row of body.rows ?? []) {
        if (row.status !== 'pending') continue;
        const decided = await api.put(`${API_BASE}/admin/moderation_status/administration`, {
          data: { id: row.id, status: 'approved' },
        });
        if (!decided.ok()) {
          // eslint-disable-next-line no-console -- see above
          console.warn(`model-request teardown: ${entity} stays pending (${String(decided.status())})`);
        }
      }
    }
  } finally {
    await api.dispose();
  }
});

test('J20g: a member files a model-connection request from AI Configuration, and the server holds it pending', async ({ page }, testInfo) => {
  // Unique per run AND per browser project: chromium and webkit drive this file
  // against ONE database at the same time, and nothing ever deletes a
  // moderation row, so a fixed name would match two rows on the second run.
  const providerType = `autotest_provider_${testInfo.project.name}_${Date.now()}`;
  // Built the way the dialog builds it. The name is alphanumeric-with-
  // underscores, so `encodeURIComponent` is the identity here and the value the
  // admin queue renders back is this exact string.
  const entity = `provider:${providerType}`;
  const description = `Journey 20g needs ${providerType} for the review pipeline.`;

  await page.goto(BASE_URL + '/app/settings/model-configuration');

  // A genuine round-trip landmark on the way in: "Create configuration" renders
  // only inside `ConfigurationsPanel`, which mounts only once all seven
  // per-section configuration queries have settled. Waiting on it means the
  // page under test is the real one and not a heading-only shell.
  await expect(page.getByRole('button', { name: 'Create configuration', exact: true })).toBeEnabled({
    timeout: 30_000,
  });

  const requestButton = page.getByRole('button', { name: 'Request a model connection' });
  await expect(
    requestButton,
    'the affordance is ungated on purpose — a member who cannot create a configuration asks for one here',
  ).toBeVisible();
  await requestButton.click();

  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible();
  // What the dialog PROMISES, and the thing the server-side test
  // `TestApprovingAModelConnectionProvisionsNothing` pins from the other end:
  // approval is clerical. If this sentence ever stops being true, this line has
  // to be deleted deliberately.
  await expect(
    dialog.getByText('it does not create the configuration for you', { exact: false }),
  ).toBeVisible();

  await checkA11y(page);

  // The provider half of the radio is the default; filling `Provider type *`
  // rather than `Model name *` is itself the assertion that it is.
  await page.getByLabel('Provider type *').fill(providerType);
  await page.getByLabel('Description *').fill(description);

  const [created] = await Promise.all([
    page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' &&
        response.url().includes('/admin/moderation_status/default/'),
      { timeout: 20_000 },
    ),
    dialog.getByRole('button', { name: 'Send request' }).click(),
  ]);
  expect(
    created.status(),
    'filing a model-connection request must be authorised (admin.moderation.create) and CREATED',
  ).toBe(201);
  filedEntities.push(entity);

  // The entity travelled as one path segment, under the `provider:` prefix that
  // is what makes two people asking for the same provider land on one address.
  expect(new URL(created.url()).pathname).toBe(
    `/api/v2/admin/moderation_status/default/${DEFAULT_PROJECT_ID}/${entity}`,
  );

  // The dialog closes only on the success branch, so both of these are
  // mutation assertions rather than paint checks.
  await expect(page.getByText('Your model connection request has been sent')).toBeVisible();
  await expect(dialog).toHaveCount(0);

  /* ── server read-back, through the caller's own request list ───────────── */
  // NOT the toast, and not the mutation's own response body: this is a second,
  // independent read of the row, filtered on the server by both the entity and
  // the issue type.
  const readBack = await page.request.get(
    `${API_BASE}/admin/moderation_status/default/${DEFAULT_PROJECT_ID}/${entity}` +
      `?issue_type=${encodeURIComponent(ISSUE_TYPE)}`,
  );
  expect(readBack.status(), 'a requester may read their own requests (admin.moderation.view)').toBe(200);
  const listed = (await readBack.json()) as {
    total?: number;
    rows?: { status?: string; issue_type?: string; description?: string; entity_id?: string; user_email?: string }[];
  };
  expect(listed.total, 'exactly the one request this test filed').toBe(1);
  const row = listed.rows?.[0];
  // `pending` and not `approved`: the server takes the status from its own
  // constant, never from the body, and the stub this route replaced answered
  // `{"status":"approved"}` to everyone.
  expect(row?.status, 'a freshly filed request is pending').toBe('pending');
  // The label the operator's queue filters on. A request filed with any other
  // issue type is created, answered 201, and lands in the WRONG queue with
  // nothing on either screen saying so.
  expect(row?.issue_type).toBe(ISSUE_TYPE);
  expect(row?.entity_id).toBe(entity);
  expect(row?.description, 'the requester owns the description, and it is stored verbatim').toBe(description);
  // Authorship is taken from the session, not from the body — and it is what
  // decides who the decision notification is delivered to.
  expect(row?.user_email).toBe('e2e-member@autotest.local');

  // The fail-open case in one line: an entity nobody has asked about must have
  // no rows, not an approval.
  const unasked = await page.request.get(
    `${API_BASE}/admin/moderation_status/default/${DEFAULT_PROJECT_ID}/provider:autotest_never_requested`,
  );
  expect(unasked.status()).toBe(200);
  expect(
    ((await unasked.json()) as { total?: number }).total,
    'an entity nobody requested has no rows, and certainly no approval',
  ).toBe(0);
});
