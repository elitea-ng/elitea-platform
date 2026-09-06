/**
 * Journey 34: The admin App Requests queue reads the table the product writes,
 *             and a decision on one really lands (JRNY-034)
 *
 * ## Why this journey needs to exist at all
 *
 * Before unit A14 every endpoint behind this page answered 200 while doing
 * nothing, so "the page renders and a table is present" was true of a
 * completely unwired stack:
 *
 *  - the queue read was a `_ *http.Request` stub returning a fixed empty page,
 *    mounted UNGATED;
 *  - the decision (`PUT /admin/moderation_status/administration`) had no route
 *    at all, so approve and reject were buttons with nothing behind them;
 *  - the product-side pair that FILLS the queue answered `{"status":"approved"}`
 *    to every caller for every entity, and its POST created nothing.
 *
 * So the assertions here are against things only a working server can produce:
 *
 *  - `e2e_app_request_probe_<engine>` is seeded into `centry.moderation_state`
 *    by `scripts/e2e-stack.sh seed` and exists in no bundle. Seeing it proves
 *    the queue reads that table.
 *  - It is authored by the MEMBER persona while the page is viewed by the ADMIN
 *    one, so the requester's address on screen proves the `auth_core__user` join
 *    ran — a listing that skipped it looks identical until a real address is
 *    expected.
 *  - Decisions are verified by a RELOAD and a re-read, never by the toast.
 *  - Both halves of the security boundary are exercised as FORGED requests,
 *    because neither is visible in the UI at all: a requester must not be able
 *    to file an already-approved request, and a moderator must not be able to
 *    rewrite what was asked while answering it.
 *  - The product side is driven end to end: a request filed through
 *    `POST /admin/moderation_status/default/1/{entity}` must appear in the
 *    operator's queue. That is the connection the two stubs used to hide.
 *
 * ## How this stays re-runnable
 *
 * A decided request cannot be returned to `pending`, deliberately. So the seeded
 * row is treated as READ-ONLY by every test here (J34 renders it, J34c aims
 * refused forgeries at it and asserts nothing moved), and each test that needs a
 * decidable row FILES ITS OWN with a run-unique key and leaves it DECIDED. The
 * Pending tab therefore ends every run containing exactly what it started with.
 *
 * ## What this run leaves behind, and what reads past it (issue #544)
 *
 * Nothing, now. Each run used to add three rows per browser project to
 * `centry.moderation_state`, for ever, on a stack that is not re-created:
 * `moderation_status` had a POST, a GET and a decision PUT, and no DELETE at any
 * mode. Measured: 20 runs of the webkit journeys on one stack left 111 rows, and
 * runs 19 and 20 failed.
 *
 * Two corrections, because the leak and the failure are different faults:
 *
 *  - `probeRow` reads with the SERVER's `entity_id` filter. It used to read
 *    `?limit=100&offset=0` and `find()` its row in the answer, and the queue
 *    sorts `created_at ASC` by default — so the newest row, which is the one
 *    every test here asks about, is the first to fall off the end. The
 *    assertion said "must be present in the queue" and meant "is on page one".
 *    A read that names the row cannot be beaten by the size of the table.
 *  - `afterAll` DELETES every row this run filed, through
 *    `DELETE /admin/moderation_status/default/1/{entity}` — the withdraw route,
 *    added for this issue beside the POST that files a request
 *    (services/elitea-main/internal/api/v2/moderation/requests.go). The delete
 *    is scoped to the AUTHOR's own rows in SQL, so each row is withdrawn through
 *    the session that filed it, and the removal is then PROVED through the
 *    operator's queue rather than read from the delete's own status code.
 *    J34g asserts the same round trip where a person can see it: the row leaves
 *    the grid after a full reload.
 *
 * `scripts/e2e-stack.sh seed` keeps its sweep. It is what removes the rows that
 * runs made BEFORE this correction left behind, and the rows of a run that dies
 * between the POST and the teardown.
 */
import { test as adminTest, expect, request as apiRequest, type APIRequestContext, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

// Serial, and not because these are slow: J34 asserts the seeded row is pending
// and J34c aims writes at its id, so a concurrent test that decided it would
// make J34 fail on scheduling luck. This orders them within a browser project;
// `seededEntity` and `runKey` below keep the two projects apart, since `serial`
// does nothing across them.
adminTest.describe.configure({ mode: 'serial' });

/** Seeded by `scripts/e2e-stack.sh seed` — see its `centry.moderation_state` block. */
function seededEntity(projectName: string): string {
  return `e2e_app_request_probe_${projectName}`;
}
function seededLabel(projectName: string): string {
  return `E2E Probe ${projectName}`;
}

/**
 * A key unique to this run AND this browser engine.
 *
 * Unique per run because a row a previous run left behind may still exist — its
 * teardown removes what it filed, and a run that died before the teardown
 * removes nothing — so a fixed key could match two rows and every
 * `toHaveCount(1)` would fail on the next run against the same stack.
 */
const RUN_ID = `${Date.now()}`;
function runEntity(projectName: string, suffix: string): string {
  return `e2e_app_request_probe_${projectName}_${suffix}_${RUN_ID}`;
}

const REQUESTER = 'e2e-member@autotest.local';
const QUEUE_URL = '/api/v2/admin/moderation_statuses/administration';
const DECISION_URL = '/api/v2/admin/moderation_status/administration';

/**
 * One request this run filed, and the session that filed it.
 *
 * The persona is recorded because the withdraw is scoped to the AUTHOR's own
 * rows in SQL: the admin's session cannot delete the row the member filed in
 * J34f, and must not be able to. A flat list of names would leave that row
 * behind and report success.
 */
interface FiledRequest {
  readonly entity: string;
  /** The persona storage state whose session created the row. */
  readonly author: string;
}

/**
 * The requests this run filed, so the teardown can remove them again.
 *
 * Filled by `fileRequest` on a 201 only: a refused POST created nothing, and a
 * teardown that looked for it would report a missing row as a fault.
 */
const filedEntities: FiledRequest[] = [];

/** `DELETE /admin/moderation_status/default/1/{entity}` — the withdraw (#544). */
function withdrawURL(entity: string): string {
  return `/api/v2/admin/moderation_status/default/1/${entity}`;
}

/**
 * Removes every row this run filed (issue #544).
 *
 * Each row is deleted through the session that CREATED it, because the route
 * refuses to reach another person's request — that scope is what stops a
 * moderator erasing the record of what they answered, and it is asserted
 * server-side by TestWithdrawCannotReachAnotherPersonsRequest.
 *
 * THE PROOF IS THE READ-BACK, NOT THE STATUS CODE. A 404 from the delete means
 * the row is already gone, which is what this hook wants; a 200 that removed
 * somebody else's row would be the same 200. So every entity is re-read through
 * the OPERATOR's queue afterwards, and a row that survives is named.
 *
 * Best effort, and LOUD about what it could not do. A teardown that fails the
 * run for a transient 503 turns a green suite red for a reason that is not the
 * product's; a teardown that hides the same 503 leaves the next reader with a
 * stray row and no account of where it came from.
 */
adminTest.afterAll(async () => {
  if (filedEntities.length === 0) return;
  // Its OWN request contexts: `page` belongs to a test, and the test this hook
  // has to clean up after may be the one whose page is already gone. One
  // context per persona, reused, because a context is a browser-sized object.
  const contexts = new Map<string, APIRequestContext>();
  const contextFor = async (state: string): Promise<APIRequestContext> => {
    const existing = contexts.get(state);
    if (existing) return existing;
    const created = await apiRequest.newContext({ baseURL: BASE_URL, storageState: state });
    contexts.set(state, created);
    return created;
  };

  try {
    // The queue read is gated on `admin.moderation`, which only the operator
    // holds — so the delete speaks as the author and the proof speaks as the
    // admin.
    const operator = await contextFor(STORAGE_STATE.admin);
    for (const filed of filedEntities) {
      const author = await contextFor(filed.author);
      const removed = await author.delete(withdrawURL(filed.entity));
      // 404 = nothing left to remove. A test that withdrew its own row (J34g)
      // reaches this hook that way, and that is the outcome this hook wants.
      if (!removed.ok() && removed.status() !== 404) {
        // eslint-disable-next-line no-console -- a silent teardown is how the rows accumulated
        console.warn(
          `J34 teardown: ${filed.entity} was not withdrawn (${String(removed.status())})`,
        );
      }

      const readBack = await operator.get(queueByEntity(filed.entity));
      if (!readBack.ok()) {
        // eslint-disable-next-line no-console -- see above
        console.warn(
          `J34 teardown: cannot read ${filed.entity} back (${String(readBack.status())})`,
        );
        continue;
      }
      const body = (await readBack.json()) as { rows?: { id: number }[] };
      if ((body.rows ?? []).length > 0) {
        // eslint-disable-next-line no-console -- see above
        console.warn(
          `J34 teardown: ${filed.entity} still has ${String((body.rows ?? []).length)} row(s) in the queue`,
        );
      }
    }
  } finally {
    for (const context of contexts.values()) await context.dispose();
  }
});

async function openAppRequests(page: Page): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/app-requests', {
    waitUntil: 'domcontentloaded',
  });
  expect(
    response?.status(),
    'the admin SPA must serve the app-requests route, not 404',
  ).toBeLessThan(400);
  await expect(page.getByRole('grid')).toBeVisible({ timeout: 20_000 });
}

/** The POST the application catalogue's "Request Access" button makes. */
async function fileRequest(
  page: Page,
  entity: string,
  label: string,
): Promise<{ status: number; body: string }> {
  const filed = await page.evaluate(
    async ([target, issueType]) => {
      const response = await fetch(`/api/v2/admin/moderation_status/default/1/${target}`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          issue_type: issueType,
          description: 'Filed by journey 34.',
          // Both shipped clients send these two, so the server must keep
          // tolerating them even though it applies neither.
          status: 'pending',
          meta: {},
        }),
      });
      return { status: response.status, body: await response.text() };
    },
    [entity, label] as const,
  );
  // Recorded here, next to the call that creates the row, so a test added later
  // cannot file one the teardown does not know about. `page` is the ADMIN
  // persona's, and the withdraw is scoped to the author, so the author is the
  // admin for every row filed through this helper.
  if (filed.status === 201) filedEntities.push({ entity, author: STORAGE_STATE.admin });
  return filed;
}

/**
 * The queue URL that asks for ONE request by name (issue #544).
 *
 * `entity_id` is an exact server-side filter — `queueFilters` renders it as
 * `m.entity_id = $n` — so the answer is that request's rows and nothing else.
 * `limit` stays because one entity can carry more than one row: a second user
 * asking for the same catalogue entry is a second row with the same
 * `entity_id`, and a RETRY of a test in this file files a second row under the
 * key its first attempt used.
 *
 * `sort_order=desc` for that second case. The queue's default is `created_at`
 * ASCENDING, so the first match would be the attempt that failed — pending,
 * while the retry the test is judging is approved. Newest first makes the
 * caller read the row it just wrote.
 */
function queueByEntity(entity: string): string {
  return (
    `${QUEUE_URL}?entity_id=${encodeURIComponent(entity)}` +
    '&sort_by=created_at&sort_order=desc&limit=100&offset=0'
  );
}

/**
 * Reads a request's real row straight off the queue endpoint.
 *
 * THE READ NAMES THE ROW (issue #544). It used to ask for `?limit=100&offset=0`
 * and `find()` the row in the answer, which holds only while the whole table is
 * shorter than 100 rows — and the queue's default order is `created_at ASC`, so
 * the row this file just filed is the last one in, and the first one out. On a
 * stack that had run the journeys 19 times the assertion below reported
 * `Received: null` and read as a queue that had lost a request.
 */
async function probeRow(
  page: Page,
  entity: string,
): Promise<{ id: number; status: string; rejection_comment: string | null }> {
  const read = await page.evaluate(
    async ([url, wanted]) => {
      const response = await fetch(url, { credentials: 'include' });
      const body = (await response.json()) as {
        total?: number;
        rows?: { id: number; entity_id: string; status: string; rejection_comment: string | null }[];
      };
      return {
        status: response.status,
        total: body.total ?? 0,
        returned: body.rows?.length ?? 0,
        row: body.rows?.find((candidate) => candidate.entity_id === wanted) ?? null,
      };
    },
    [queueByEntity(entity), entity] as const,
  );

  expect(read.status, `the queue read for ${entity} must be authorised`).toBe(200);
  expect(
    read.row,
    `the request ${entity} must be present in the queue. The filtered read answered ` +
      `total=${String(read.total)} with ${String(read.returned)} rows: a total larger than ` +
      `the rows returned means the server ignored entity_id, so this is a paging miss and ` +
      `not a missing request (#544).`,
  ).not.toBeNull();
  return read.row as { id: number; status: string; rejection_comment: string | null };
}

adminTest('J34: the queue is the moderation table, read from the database', async ({ page }, testInfo) => {
  const entity = seededEntity(testInfo.project.name);
  await openAppRequests(page);

  // The read is gated on `admin.moderation`; the admin persona holds it via the
  // seed, so this banner appearing would mean the gate and the seed drifted.
  await expect(page.getByTestId('admin-app-requests-unavailable')).toHaveCount(0);

  const row = page.getByRole('row').filter({ hasText: entity });
  await expect(row).toHaveCount(1, { timeout: 20_000 });
  // The requester's ADDRESS, resolved by joining auth_core__user. The row itself
  // carries only a user id, and the member persona is not the one viewing this
  // page — so this string can only have come from the join.
  await expect(row.getByText(REQUESTER)).toBeVisible();
  // The catalogue key AND the label the requesting client displayed. The
  // reference page renders only the key.
  await expect(row.getByText(seededLabel(testInfo.project.name))).toBeVisible();
  await expect(row.getByText('Pending')).toBeVisible();

  await checkA11y(page);
});

adminTest('J34b: a request filed through the PRODUCT endpoint reaches the queue', async ({ page }, testInfo) => {
  const entity = runEntity(testInfo.project.name, 'filed');
  await openAppRequests(page);

  const filed = await fileRequest(page, entity, 'E2E Filed Probe');
  expect(filed.status, 'filing an access request must be authorised and created').toBe(201);

  // A FULL RELOAD, not a cache read: this is the assertion a POST that answers
  // 201 and writes nothing cannot pass.
  await page.reload({ waitUntil: 'domcontentloaded' });
  const row = page.getByRole('row').filter({ hasText: entity });
  await expect(row).toHaveCount(1, { timeout: 20_000 });
  await expect(row.getByText('E2E Filed Probe')).toBeVisible();

  // Reading one's own requests back answers the CALLER's rows, not a constant.
  // The stub answered `{"status":"approved"}` to this exact call.
  const readBack = await page.evaluate(async (target) => {
    const response = await fetch(`/api/v2/admin/moderation_status/default/1/${target}`, {
      credentials: 'include',
    });
    return (await response.json()) as { total?: number; rows?: { status: string }[] };
  }, entity);
  expect(readBack.total, 'the caller must see the request they just filed').toBe(1);
  expect(readBack.rows?.[0]?.status, 'a fresh request is pending, never approved').toBe('pending');

  // An entity nobody has asked about must NOT come back approved — the
  // fail-open case in one line.
  const unasked = await page.evaluate(async () => {
    const response = await fetch(
      '/api/v2/admin/moderation_status/default/1/e2e_never_requested_entity',
      { credentials: 'include' },
    );
    return (await response.json()) as { total?: number };
  });
  expect(unasked.total, 'an entity nobody requested must have no rows, not an approval').toBe(0);

  // Leave the queue as it was found: decided, so the Pending tab does not grow
  // by one row per run.
  await row.getByRole('button', { name: 'Approve request: E2E Filed Probe' }).click();
  await expect(page.getByTestId('admin-app-requests-saved')).toBeVisible();
});

adminTest('J34c: a requester cannot decide their own request, and a moderator cannot rewrite it', async ({ page }, testInfo) => {
  const entity = seededEntity(testInfo.project.name);
  await openAppRequests(page);
  const before = await probeRow(page, entity);

  // Half one: SELF-APPROVAL. pylon's create model declares
  // `status: ModerationStatus = PENDING`, which is a default and not a
  // restriction, so a body carrying "approved" is stored verbatim there — any
  // project member could file a request that arrives already decided.
  const selfApproved = await page.evaluate(async (target) => {
    const response = await fetch(`/api/v2/admin/moderation_status/default/1/${target}`, {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        issue_type: 'E2E Forged Probe',
        description: 'Already approved, thanks.',
        status: 'approved',
      }),
    });
    return { status: response.status, body: await response.text() };
  }, runEntity(testInfo.project.name, 'forged'));
  expect(
    selfApproved.status,
    'a requester deciding their own request must be refused by the SERVER, not merely absent from the UI',
  ).toBe(400);
  expect(selfApproved.body).toContain('status');

  // Half two: the MODERATOR rewriting what was asked. If the decision endpoint
  // could edit `entity_id`, an approved row would stop being evidence of what
  // was approved; `meta` is refused because the endpoint REPLACES it, so a
  // decision would destroy what the requester stored.
  for (const forgery of [
    { entity_id: 'something_else_entirely' },
    { issue_type: 'Something Else' },
    { description: 'rewritten after the fact' },
    { meta: { granted: true } },
    { user_id: 1 },
  ]) {
    const refused = await page.evaluate(
      async ([url, id, extra]) => {
        const response = await fetch(url as string, {
          method: 'PUT',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ id, status: 'approved', ...(extra as object) }),
        });
        return { status: response.status, body: await response.text() };
      },
      [DECISION_URL, before.id, forgery] as const,
    );
    expect(
      refused.status,
      `a decision carrying ${Object.keys(forgery)[0]} must be refused, not silently ignored`,
    ).toBe(400);
  }

  // Half three: a rejection with no reason. pylon's field validator does not fire
  // when the key is ABSENT, so a null-reason rejection gets through there — and
  // a bare refusal is all the requester is ever told.
  const reasonless = await page.evaluate(
    async ([url, id]) => {
      const response = await fetch(url as string, {
        method: 'PUT',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id, status: 'rejected' }),
      });
      return { status: response.status, body: await response.text() };
    },
    [DECISION_URL, before.id] as const,
  );
  expect(reasonless.status, 'a rejection with no reason must be refused').toBe(400);
  expect(reasonless.body).toContain('rejection_comment');

  // Nothing moved through any of that — which is also what keeps the seeded row
  // usable by the next run.
  expect((await probeRow(page, entity)).status, 'a refused decision must not have applied').toBe(
    'pending',
  );
});

adminTest('J34d: approving reaches the server and survives a full reload', async ({ page }, testInfo) => {
  const entity = runEntity(testInfo.project.name, 'approve');
  const label = 'E2E Approve Probe';
  await openAppRequests(page);
  expect((await fileRequest(page, entity, label)).status).toBe(201);

  await page.reload({ waitUntil: 'domcontentloaded' });
  const row = page.getByRole('row').filter({ hasText: entity });
  await expect(row).toHaveCount(1, { timeout: 20_000 });
  await expect(row.getByText('Pending')).toBeVisible();

  // The response status is what proves the request was AUTHORISED, not merely
  // sent — the admin persona holds `admin.moderation.edit` via the seed.
  const [decision] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/admin/moderation_status/administration') && r.request().method() === 'PUT',
    ),
    row.getByRole('button', { name: `Approve request: ${label}` }).click(),
  ]);
  expect(decision.status(), 'the decision must be authorised server-side').toBe(200);

  // The page says what approving DOES, and what it does is notify the requester.
  await expect(page.getByTestId('admin-app-requests-saved')).toContainText('notified');

  // A FULL RELOAD, on the tab that only shows approved rows. This is the
  // assertion a handler that answers 200 and writes nothing cannot pass.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.getByRole('tab', { name: 'Approved', exact: true }).click();
  await expect(page.getByRole('row').filter({ hasText: entity })).toHaveCount(1, {
    timeout: 20_000,
  });

  // …and it is gone from Pending, so the status filter is the SERVER's and not a
  // client-side slice of one page.
  await page.getByRole('tab', { name: 'Pending', exact: true }).click();
  await expect(page.getByRole('row').filter({ hasText: entity })).toHaveCount(0);

  expect((await probeRow(page, entity)).status).toBe('approved');
});

adminTest('J34e: rejecting requires a reason, and the reason is rendered back', async ({ page }, testInfo) => {
  const entity = runEntity(testInfo.project.name, 'reject');
  const label = 'E2E Reject Probe';
  const reason = 'Not licensed for this tenant.';

  await openAppRequests(page);
  expect((await fileRequest(page, entity, label)).status).toBe(201);

  await page.reload({ waitUntil: 'domcontentloaded' });
  const row = page.getByRole('row').filter({ hasText: entity });
  await expect(row).toHaveCount(1, { timeout: 20_000 });

  await row.getByRole('button', { name: `Reject request: ${label}` }).click();
  // The dialog names the row, so an operator working a queue can see which one
  // they are about to refuse. The reference dialog shows only "Reject Request".
  await expect(page.getByTestId('admin-app-requests-reject-subject')).toContainText(label);

  // Confirming with an empty reason must not reach the server: it would be a
  // request the client already knows is refused.
  await page.getByTestId('admin-app-requests-reject-confirm').click();
  await expect(page.getByText('A reason is required.')).toBeVisible();

  await page.getByTestId('admin-app-requests-reject-reason').fill(reason);
  const [decision] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/admin/moderation_status/administration') && r.request().method() === 'PUT',
    ),
    page.getByTestId('admin-app-requests-reject-confirm').click(),
  ]);
  expect(decision.status()).toBe(200);

  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.getByRole('tab', { name: 'Rejected', exact: true }).click();
  const rejected = page.getByRole('row').filter({ hasText: entity });
  await expect(rejected).toHaveCount(1, { timeout: 20_000 });
  // The reason is rendered BACK. The reference page has no column for it, so an
  // operator's own words are invisible the moment the dialog closes.
  await expect(rejected.getByText(reason, { exact: false })).toBeVisible();

  expect((await probeRow(page, entity)).rejection_comment).toBe(reason);

  await checkA11y(page);
});

adminTest('J34g: a withdrawn request leaves the queue, and the seeded row stays (#544)', async ({ page }, testInfo) => {
  const entity = runEntity(testInfo.project.name, 'withdraw');
  const label = 'E2E Withdraw Probe';
  const seeded = seededEntity(testInfo.project.name);

  await openAppRequests(page);
  expect((await fileRequest(page, entity, label)).status).toBe(201);

  // The row is really there first. A journey that asserted only the ABSENCE
  // below would pass against a queue that never showed the row at all.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('row').filter({ hasText: entity })).toHaveCount(1, {
    timeout: 20_000,
  });

  // The withdraw, through the same route family that filed it. The status is
  // read here because the list contents below are the real assertion — a 200
  // that removed nothing cannot pass them.
  const withdrawn = await page.evaluate(async (target) => {
    const response = await fetch(`/api/v2/admin/moderation_status/default/1/${target}`, {
      method: 'DELETE',
      credentials: 'include',
    });
    return { status: response.status, body: await response.text() };
  }, entity);
  expect(
    withdrawn.status,
    `withdrawing a request must be authorised and applied: ${withdrawn.body.slice(0, 300)}`,
  ).toBe(200);

  // A FULL RELOAD. This is the assertion a delete that answers 200 and removes
  // nothing cannot pass — and it is this issue's fault from the other side: the
  // rows that could not be removed at all.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('row').filter({ hasText: seeded })).toHaveCount(1, {
    timeout: 20_000,
  });
  // …and the withdrawn one is gone. Both counts in one state: a delete that
  // emptied the queue fails the line above, and one that removed nothing fails
  // this line.
  await expect(page.getByRole('row').filter({ hasText: entity })).toHaveCount(0);

  // Gone from the TABLE, not only from the tab this page opens on.
  const readBack = await page.evaluate(async (url) => {
    const response = await fetch(url, { credentials: 'include' });
    const body = (await response.json()) as { total?: number };
    return { status: response.status, total: body.total ?? 0 };
  }, queueByEntity(entity));
  expect(readBack.status).toBe(200);
  expect(readBack.total, 'the withdrawn request must leave no row behind').toBe(0);

  // A second withdraw is a 404, not a 200 that removed nothing. A teardown that
  // read success from an empty delete would report a clean stack while the
  // table kept growing, which is how this issue stayed invisible for 18 runs.
  const again = await page.evaluate(async (target) => {
    const response = await fetch(`/api/v2/admin/moderation_status/default/1/${target}`, {
      method: 'DELETE',
      credentials: 'include',
    });
    return response.status;
  }, entity);
  expect(again, 'a withdraw that removes nothing must not answer 200').toBe(404);
});

/* ───────────────────────────────────────────────────────────────────────────
 * MODEL CONNECTION REQUESTS — the second kind of request this one queue holds
 *
 * Settings › AI Configuration files into `centry.moderation_state` over the
 * SAME create call the App Catalogue's "Request Access" card uses. There is no
 * new route and no new column; the ONLY thing separating the two populations is
 * `issue_type`, which is why the operator's issue-type filter is part of the
 * feature rather than a convenience — without it a model-connection request and
 * an app-access request are indistinguishable rows in one list.
 *
 * The three assertions below are the ones nothing else in this repository can
 * make:
 *
 *  - the filter narrows SERVER-side. `queueFilters` has always accepted
 *    `issue_type`; no client sent it until this control existed, so a
 *    client-side slice of one page would look identical on a short queue and
 *    silently hide every match past the page boundary on a long one.
 *  - the decision notifies the REQUESTER, who is not the person clicking. The
 *    row carries a user id; the notification has to land on that user, in that
 *    project, and it is read back through the requester's own session.
 *  - approval is CLERICAL. Nothing is provisioned. That is the promise the
 *    dialog makes to the requester ("it does not create the configuration for
 *    you"), it is pinned server-side by
 *    `TestApprovingAModelConnectionProvisionsNothing`, and it is pinned from
 *    the outside here: no configuration row appears for the requested provider,
 *    and no existing row's `status_ok` moves.
 * ─────────────────────────────────────────────────────────────────────────── */

/** The `issue_type` `RequestModelConnection.tsx` files under, byte for byte. */
const MODEL_CONNECTION_ISSUE_TYPE = 'Model Connection Request';

/** The requester's own notification list — the same route the app's bell reads. */
const NOTIFICATIONS_URL =
  '/api/v2/notifications/notifications/prompt_lib/1?limit=100&event_type=moderation_approved'; // #544: as narrow as the route filters; every row's meta.entity_id is then asserted below

/** One notification as both registrations of that surface report it (`{total, rows}` either way). */
interface NotificationRow {
  readonly event_type: string;
  readonly is_seen: boolean;
  readonly project_id?: number;
  readonly meta: { readonly entity_id?: string; readonly issue_type?: string; readonly message?: string } | null;
}

/**
 * Every configuration this project can see, as `id → status_ok`.
 *
 * BOTH lists: a project's own rows and the platform-shared ones it inherits.
 * The clerical pin is about what approving a request does to the configuration
 * table, and a row appearing in either half would be provisioning.
 */
interface ConfigurationSnapshot {
  readonly statusById: ReadonlyMap<string, boolean>;
  readonly names: readonly string[];
}

async function readConfigurations(api: APIRequestContext): Promise<ConfigurationSnapshot> {
  const response = await api.get('/api/v2/configurations/configurations/1?limit=100&offset=0'); // #544: unfiltered ON PURPOSE — the clerical pin must see EVERY row (an entity_id filter would hide the very row a provisioning bug would create); reads are id-intersected below
  expect(response.status(), 'the clerical pin needs a readable configuration list').toBe(200);
  const body = (await response.json()) as {
    items?: { id?: number | string; name?: string; label?: string; status_ok?: boolean }[];
    shared?: { items?: { id?: number | string; name?: string; label?: string; status_ok?: boolean }[] };
  };
  const rows = [...(body.items ?? []), ...(body.shared?.items ?? [])];
  const statusById = new Map<string, boolean>();
  const names: string[] = [];
  for (const row of rows) {
    if (row.id !== undefined) statusById.set(String(row.id), row.status_ok === true);
    if (row.name !== undefined) names.push(row.name);
    if (row.label !== undefined) names.push(row.label);
  }
  return { statusById, names };
}

/**
 * Opens the issue-type filter and picks one label, returning the queue read the
 * choice caused.
 *
 * The response is what the assertions are made against, not the grid: the grid
 * would look the same whether the narrowing happened on the server or in the
 * browser, and only one of those survives a queue longer than a page.
 */
async function selectIssueType(page: Page, issueType: string) {
  const filter = page.getByTestId('admin-app-requests-issue-type-filter');
  await expect(filter).toBeVisible();
  const [response] = await Promise.all([
    page.waitForResponse(
      (candidate) =>
        candidate.url().includes('/admin/moderation_statuses/administration') &&
        candidate.url().includes('issue_type='),
      { timeout: 20_000 },
    ),
    (async () => {
      await filter.getByRole('combobox').click();
      await page.getByRole('option', { name: issueType, exact: true }).click();
    })(),
  ]);
  return response;
}

adminTest('J34f: a model-connection request is isolated by issue type, approved, and notifies its requester — while provisioning nothing', async ({ page }, testInfo) => {
  // The MEMBER files it and the ADMIN decides it, which is the whole point of
  // the notification assertion: the two are different people, and the row
  // carries the requester's id rather than the decider's.
  const providerType = `autotest_mc_${testInfo.project.name}_${RUN_ID}`;
  const entity = `provider:${providerType}`;
  const description = 'Journey 34f: the member persona needs this provider.';

  const member = await apiRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.member });
  try {
    const filed = await member.post(`/api/v2/admin/moderation_status/default/1/${entity}`, {
      data: { issue_type: MODEL_CONNECTION_ISSUE_TYPE, description },
    });
    expect(
      filed.status(),
      `the member must be able to file: ${(await filed.text()).slice(0, 300)}`,
    ).toBe(201);
    // Recorded next to the call that created it, so the file-level teardown
    // removes it even if this test fails before the approve below. The MEMBER
    // filed it, so only the member's session can withdraw it.
    filedEntities.push({ entity, author: STORAGE_STATE.member });

    // The clerical pin's BEFORE read, taken before the operator sees the queue.
    const before = await readConfigurations(member);

    await openAppRequests(page);

    /* ── the filter narrows on the server ─────────────────────────────────── */
    const filtered = await selectIssueType(page, MODEL_CONNECTION_ISSUE_TYPE);
    expect(filtered.status(), 'the filtered queue read must be authorised').toBe(200);
    const filteredBody = (await filtered.json()) as { rows?: { issue_type: string }[] };
    const issueTypes = [...new Set((filteredBody.rows ?? []).map((row) => row.issue_type))];
    expect(
      issueTypes,
      'the SERVER answered only rows of the chosen issue type — a client-side slice would carry the others too',
    ).toEqual([MODEL_CONNECTION_ISSUE_TYPE]);

    // The choice is shareable: it is mirrored into the URL with replaceState, so
    // a reload restores it and an operator can hand the link to a colleague.
    await expect(page).toHaveURL(/[?&]issue_type=Model\+Connection\+Request/);

    /* ── the grid shows this request, and not the other population ────────── */
    // `provider:<type>` is rendered as a readable "Provider: <type>" line —
    // every other entity_id on this deployment passes through unchanged, so
    // this string exists only because the convention was recognised.
    const row = page.getByRole('row').filter({ hasText: `Provider: ${providerType}` });
    await expect(row).toHaveCount(1, { timeout: 20_000 });
    await expect(row.getByText(MODEL_CONNECTION_ISSUE_TYPE)).toBeVisible();
    // The requester's ADDRESS, resolved by joining auth_core__user. The person
    // reading this page is the admin, so this string can only be the join's.
    await expect(row.getByText(REQUESTER)).toBeVisible();
    // …and the app-access population is gone from the view. Asserting only the
    // presence of the match above would pass against a filter that narrowed
    // nothing at all.
    await expect(
      page.getByRole('row').filter({ hasText: seededEntity(testInfo.project.name) }),
      'the app-access probe belongs to another issue type and must not be in this view',
    ).toHaveCount(0);

    await checkA11y(page);

    /* ── the decision ─────────────────────────────────────────────────────── */
    const [decision] = await Promise.all([
      page.waitForResponse(
        (candidate) =>
          candidate.url().includes('/admin/moderation_status/administration') &&
          candidate.request().method() === 'PUT',
      ),
      row.getByRole('button', { name: `Approve request: ${MODEL_CONNECTION_ISSUE_TYPE}` }).click(),
    ]);
    expect(decision.status(), 'the decision must be authorised server-side (admin.moderation.edit)').toBe(200);
    await expect(page.getByTestId('admin-app-requests-saved')).toContainText('notified');

    /* ── (a) the queue read-back ──────────────────────────────────────────── */
    expect(
      (await probeRow(page, entity)).status,
      'the decision must have landed in the table, not only in the toast',
    ).toBe('approved');

    /* ── (b) the requester's notification ─────────────────────────────────── */
    // Read through the MEMBER's session: `centry.notifications` is scoped to the
    // caller in SQL, so a notification written to the wrong user is invisible
    // here — which is exactly the failure this asserts against.
    const notified = await member.get(NOTIFICATIONS_URL);
    expect(notified.status(), 'the requester may list their own notifications').toBe(200);
    const notifications = ((await notified.json()) as { rows?: NotificationRow[] }).rows ?? [];
    const mine = notifications.filter((candidate) => candidate.meta?.entity_id === entity);
    expect(
      mine,
      'the approval writes exactly one notification, addressed to the REQUESTER and naming what was decided',
    ).toHaveLength(1);
    const notification = mine[0] as NotificationRow;
    expect(notification.event_type).toBe('moderation_approved');
    expect(notification.meta?.issue_type).toBe(MODEL_CONNECTION_ISSUE_TYPE);
    // Neither frontend's notification renderer has a branch for this event
    // type, so the sentence has to travel with the row or the requester is
    // shown an empty notification.
    expect(String(notification.meta?.message ?? '')).toContain('approved');
    expect(notification.is_seen, 'a notification written already-seen is one nobody is ever shown').toBe(false);

    /* ── (c) approval is CLERICAL ─────────────────────────────────────────── */
    const after = await readConfigurations(member);
    expect(
      after.names.filter((name) => name.includes(providerType)),
      'approving a model-connection request must not create a configuration for the requested provider',
    ).toEqual([]);
    // Every row that existed before must still carry the SAME status_ok.
    // Restricted to the ids present in both reads on purpose: other journeys
    // create and delete their own configurations in this project concurrently,
    // and their rows appearing or leaving is not this decision's doing.
    const moved: string[] = [];
    for (const [id, statusOk] of before.statusById) {
      const now = after.statusById.get(id);
      if (now !== undefined && now !== statusOk) moved.push(id);
    }
    expect(
      moved,
      'approving verifies no connection and admits no row — no existing status_ok may move',
    ).toEqual([]);
  } finally {
    await member.dispose();
  }
});
