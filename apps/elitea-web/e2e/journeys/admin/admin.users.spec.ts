/**
 * Journey 27: Admin SPA is served with server-injected config, and lists the
 *             real users from the database (JRNY-027)
 * Journey 28: An admin write reaches the database and survives a reload
 *             (JRNY-028)
 *
 * ## What changed in unit A14, and why these are now stronger
 *
 * Until A14 `src/entries/admin/main.tsx` was a ~160-line PLACEHOLDER with ZERO
 * network calls: hardcoded `DEFAULT_ROLE_PERMISSIONS`, toggles written to
 * `sessionStorage` under `el.admin.rolePermissions`, and a `user_email`
 * fallback of `'admin@example.com'`. The two journeys here were narrowed to
 * match: J27 asserted the injected `window.admin_ui_config` and that the words
 * "Elitea Admin" appeared; J28 asserted that a roles matrix rendered. Their
 * headers said, in as many words, "do NOT add persistence or
 * permission-semantics assertions here until a real admin backend exists —
 * they would be asserting the placeholder."
 *
 * That backend now exists. `GET /admin/auth_users/administration` reads
 * `auth_core__user`; `POST /admin/auth_users/administration` and
 * `PUT /admin/user_suspend/administration/{id}` write it, gated server-side on
 * the `admin.auth.users` permission resolved from `auth_core__user_role`. So
 * the constraint the old headers described is lifted, and both journeys now
 * assert against the database instead of against a placeholder.
 *
 * The roles matrix J28 used to assert is GONE — it was the placeholder's
 * sessionStorage toy, not a product surface, and A14 deleted it along with the
 * rest of that entry. J28 is repointed at the strongest thing that now exists
 * in its place: a write that must survive a full page reload. That is the
 * assertion the sessionStorage version could never make honestly, since it
 * would have passed with no admin backend in existence at all.
 */
import { test as adminTest, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

interface AdminUIConfig {
  readonly vite_server_url?: string;
  readonly vite_base_uri?: string;
  readonly user_email?: string;
  readonly permissions?: readonly string[];
  readonly roles?: readonly string[];
}

/** Seeded by `scripts/e2e-stack.sh seed`. */
const SEEDED_ADMIN = 'e2e-admin@autotest.local';
const SEEDED_MEMBER = 'e2e-member@autotest.local';

/**
 * The user journey 28 suspends — one row per browser project, and NEITHER of
 * the two personas above (issue #519).
 *
 * Journey 28 used to suspend `SEEDED_MEMBER`. That is the identity almost
 * every other journey in this suite signs in as, and elitea-main refuses a
 * suspended principal: `authsvc.PrincipalValidator` reloads the row through
 * `GetActiveUserPrincipalByID`, which filters on `suspended = false`, and the
 * request is answered 401 "authenticated principal is inactive". So for the
 * whole suspend-reload-unsuspend window, every request of every concurrently
 * running journey was answered 401 rather than what it asked for.
 *
 * `fullyParallel` is on, so which journeys were inside the window changed on
 * every run. That is measurable, not theoretical: journey 33 below reads the
 * user listing AS THE MEMBER and asserts 403, and it received 401 — the two
 * tests are in this one file, and the window is why the same tree gives a
 * different result each time.
 *
 * One row per project for the reason `admin.features.spec.ts` gives for its
 * Help Center card: both engines run this file against one platform-wide
 * table when the suite is run locally with both projects, and a shared row
 * would have each engine observing the other's suspend window.
 */
function suspendFixture(projectName: string): string {
  return projectName === 'chromium'
    ? 'e2e-suspend-chromium@autotest.local'
    : 'e2e-suspend-webkit@autotest.local';
}

/** One row of `GET /admin/auth_users/administration`, reduced to what J28 reads. */
interface ListedUser {
  readonly email: string;
  readonly suspended: boolean;
}

/**
 * Read the global user listing from the SERVER, as this browser.
 *
 * Deliberately a direct read rather than a `waitForResponse` around the
 * reload. That shape looks tighter and is wrong here: the suspend mutation
 * invalidates the list, so the CURRENT document issues its own refetch, the
 * wait matches THAT response, and the reload then discards the document it
 * belongs to — `response.json()` fails with "Response body is not available
 * for a response that was navigated away from". Measured, on the second
 * re-read of this journey.
 *
 * A direct read has no such race and answers the same question: what does the
 * server say after the write. The reload and the badge assertion that follow
 * it are untouched, so the pair still separates a write that did not land from
 * a grid that did not paint.
 */
async function serverUsers(page: Page): Promise<ListedUser[]> {
  const listing = await page.request.get(
    `${BASE_URL}/api/v2/admin/auth_users/administration?limit=200&offset=0`,
  );
  expect(listing.status(), await listing.text()).toBe(200);
  return ((await listing.json()) as { rows: ListedUser[] }).rows;
}

/**
 * Put J28's own fixture row back to `suspended = false` through the API,
 * before the journey asserts that baseline on screen.
 */
async function restoreSuspendFixture(page: Page, email: string): Promise<void> {
  const row = ((await serverUsers(page)) as (ListedUser & { id: number })[]).find(
    (r) => r.email === email,
  );
  expect(row, `the seed must carry ${email}`).toBeTruthy();
  if (row?.suspended !== true) return;
  const restored = await page.request.put(
    `${BASE_URL}/api/v2/admin/user_suspend/administration/${row.id}`,
    { data: { suspended: false } },
  );
  expect(restored.status(), await restored.text()).toBe(200);
}

/** What the server said about one address in that listing. */
function suspendedInListing(rows: ListedUser[], email: string): boolean {
  const row = rows.find((r) => r.email === email);
  expect(row, `the listing must carry ${email}`).toBeTruthy();
  return row?.suspended === true;
}

adminTest('J27: the admin SPA is served with injected config and lists database users', async ({ page }) => {
  const response = await page.goto(BASE_URL + '/admin/app/users', { waitUntil: 'domcontentloaded' });

  // The Go handler must actually serve it. A 404 here means the route mount or
  // the static dir is broken.
  expect(response?.status(), 'admin SPA must be served, not 404').toBeLessThan(400);

  // The injected config remains a real integration: handler.go replaces the
  // `<!-- admin_ui_config -->` marker at request time. If the marker, the
  // handler, or the built index.html drift apart, this is the only thing that
  // notices.
  const config = await page.evaluate(
    () => (window as unknown as { admin_ui_config?: AdminUIConfig }).admin_ui_config,
  );
  expect(config, 'window.admin_ui_config must be injected by adminui/handler.go').toBeDefined();
  expect(config?.vite_base_uri, 'vite_base_uri must name the admin base path').toContain('/admin');

  // The listing must come from the DATABASE. Both personas are seeded rows in
  // auth_core__user, and neither string appears anywhere in the bundle — the
  // placeholder rendered `window.admin_ui_config.user_email` and a literal
  // "admin" role, which is precisely why it could never have passed this.
  //
  // Scoped to <main>, which is the PAGE. The nav (#225) shows the signed-in
  // operator's own name in its footer, and this browser is signed in as the
  // admin persona — so an unscoped `getByText(SEEDED_ADMIN)` now matches the
  // nav footer too, and would pass while the table was empty. That is the
  // stronger assertion regardless: what this test claims is that the LISTING
  // came from the database.
  const listing = page.getByRole('main');
  await expect(listing.getByText(SEEDED_ADMIN)).toBeVisible({ timeout: 15_000 });
  await expect(listing.getByText(SEEDED_MEMBER)).toBeVisible();

  // The empty state and the table are mutually exclusive branches; a listing
  // that silently resolved to zero rows is the #130/#132 failure shape.
  await expect(page.getByText('No users')).toHaveCount(0);

  // The tab counts come from the same response's `counts`, which is computed
  // over ALL users and is what labels the two tabs.
  await expect(page.getByRole('tab', { name: /Platform Users \(\d+\)/ })).toBeVisible();
  await expect(page.getByRole('tab', { name: /System Users \(\d+\)/ })).toBeVisible();

  await checkA11y(page);
});

adminTest('J28: suspending a user is written to the database and survives a reload', async ({ page }, testInfo) => {
  // NOT the member persona — see `suspendFixture`'s note. Every assertion
  // below is the one this journey always made; only the row it acts on moved
  // off the identity the rest of the suite signs in as.
  const subject = suspendFixture(testInfo.project.name);

  // The journey RESTORES the row it owns before asserting the baseline,
  // rather than assuming the seed's value.
  //
  // The restore at the end of this test is the only thing that returns the row
  // to `suspended = false`, so ANY failure between the suspend and that
  // restore leaves the fixture suspended — and then this test's own
  // precondition (`Active`, below) fails on every later run and on every
  // retry, against a stack nobody re-seeded. Measured while hardening this
  // journey: one aborted run made the next three fail here, on a line that had
  // nothing to do with the abort.
  //
  // Restoring first is the same correction PR #794 made to the serial
  // platform-config group for the same reason. It weakens nothing: the
  // baseline is still ASSERTED below, on the row the server reports.
  await restoreSuspendFixture(page, subject);

  await page.goto(BASE_URL + '/admin/app/users', { waitUntil: 'domcontentloaded' });
  const subjectRow = page.getByRole('row').filter({ hasText: subject });
  await expect(subjectRow).toBeVisible({ timeout: 15_000 });

  // Precondition, asserted rather than assumed: the seed creates this user
  // un-suspended, and a test that started from "already suspended" would prove
  // nothing about the write.
  await expect(subjectRow.getByText('Active')).toBeVisible();

  const suspend = subjectRow.getByRole('button', { name: 'Suspend user' });
  await expect(suspend, 'the admin persona holds admin.auth.users, so the control must be live').toBeEnabled();

  // The response is what proves the request was AUTHORISED, not merely sent.
  // Before A14's seed change no persona held an administration-mode
  // permission, so this same click produced a 403 while the page still listed
  // users perfectly — a stack that looks working and is not.
  const [suspendResponse] = await Promise.all([
    page.waitForResponse((r) => r.url().includes('/admin/user_suspend/administration/') && r.request().method() === 'PUT'),
    suspend.click(),
  ]);
  expect(suspendResponse.status(), 'the suspend write must be authorised server-side').toBe(200);

  // A full reload, not a client-side refetch: this is the assertion a handler
  // that answers 200 and writes nothing (#130, #180) cannot pass, and the one
  // the deleted sessionStorage version of this journey only pretended to make.
  //
  // The reload's OWN listing is waited for and asserted, and the badge after
  // it (#545). The badge alone was this journey's flake, and the comment above
  // says why that mattered more here than elsewhere: the flake's shape — 200
  // asserted, badge absent after the reload — is INDISTINGUISHABLE from the
  // "answers 200 and writes nothing" defect the journey exists to catch. A
  // journey that cannot tell its target bug from a timing wobble proves
  // nothing on either.
  //
  // With the server read asserted first the two separate for good: a handler
  // that wrote nothing fails on `suspended` in the listing, and a grid that did
  // not paint a row the listing carries fails on the badge.
  expect(suspendedInListing(await serverUsers(page), subject)).toBe(true);
  await page.reload({ waitUntil: 'domcontentloaded' });
  const afterReload = page.getByRole('row').filter({ hasText: subject });
  await expect(afterReload.getByText('Suspended')).toBeVisible({ timeout: 15_000 });

  // Restore, so this journey leaves the stack as it found it and can be re-run.
  const [unsuspendResponse] = await Promise.all([
    page.waitForResponse((r) => r.url().includes('/admin/user_suspend/administration/') && r.request().method() === 'PUT'),
    afterReload.getByRole('button', { name: 'Unsuspend user' }).click(),
  ]);
  expect(unsuspendResponse.status()).toBe(200);
  expect(suspendedInListing(await serverUsers(page), subject)).toBe(false);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(
    page.getByRole('row').filter({ hasText: subject }).getByText('Active'),
  ).toBeVisible({ timeout: 15_000 });
});

adminTest('J34: the activity drawer reads the audit trail scoped to one user', async ({ page }) => {
  // The clicked row's id, resolved INDEPENDENTLY of the drawer. Reading it back
  // out of the drawer's own request instead would be circular: a drawer wired
  // to `users[0]` would query for the wrong person AND head itself with that
  // same wrong person, and every assertion below would agree with itself.
  const listing = await page.request.get(
    `${BASE_URL}/api/v2/admin/auth_users/administration?limit=100&offset=0&search=${SEEDED_MEMBER}`,
  );
  expect(listing.status(), 'the listing must answer before the drawer can be checked').toBe(200);
  const listed = (await listing.json()) as { rows?: { id: number; email: string }[] };
  const expectedId = listed.rows?.find((entry) => entry.email === SEEDED_MEMBER)?.id;
  expect(expectedId, `${SEEDED_MEMBER} must be a seeded row`).toBeGreaterThan(0);

  await page.goto(BASE_URL + '/admin/app/users', { waitUntil: 'domcontentloaded' });
  const row = page.getByRole('row').filter({ hasText: SEEDED_MEMBER });
  await expect(row).toHaveCount(1, { timeout: 20_000 });

  // The drawer reuses the deployment-wide audit endpoints. Its queries must
  // carry THIS row's user id — a drawer that dropped it would show the whole
  // platform's traces under one person's name, and the rows would look
  // perfectly plausible. This control shipped DISABLED until the per-user view
  // was ported, so "it is enabled and it queries" is the claim under test.
  const traceListing = page.waitForResponse(
    (r) =>
      r.url().includes('/elitea_core/audit_traces/administration') && r.request().method() === 'GET',
  );
  await row.getByRole('button', { name: 'User activity' }).click();

  const traceResponse = await traceListing;
  expect(traceResponse.status(), 'the audit read must be authorised server-side').toBe(200);

  // Against the id the LISTING gave, not against the drawer's own heading —
  // that is what rules out a drawer pinned to the first row, or to nothing.
  // Compared as a parsed parameter, never as a substring of the URL: `user_id=1`
  // is a substring of `user_id=12`, so `toContain` would accept a query pinned
  // to a different account whose id merely starts with these digits.
  const userId = new URL(traceResponse.url()).searchParams.get('user_id');
  expect(userId, 'the listing must be scoped to the clicked user').toBe(String(expectedId));
  await expect(page.getByText(`(ID: ${userId})`)).toBeVisible({ timeout: 15_000 });

  // The heatmap is drawn over the same window as the table beneath it, so it
  // must carry the same pin; a chart filtered differently from its table is a
  // lie about the same data.
  const heatmap = page.waitForResponse((r) =>
    r.url().includes('/elitea_core/audit_trace_heatmap/administration'),
  );
  await page.getByRole('button', { name: 'Refresh' }).click();
  const heatmapResponse = await heatmap;
  expect(heatmapResponse.status()).toBe(200);
  expect(new URL(heatmapResponse.url()).searchParams.get('user_id')).toBe(userId);

  // Spans is the strictly per-user view: with `user_id` pinned, a TRACE is any
  // trace containing one of this user's spans, so it can carry somebody else's.
  const spanListing = page.waitForResponse(
    (r) => r.url().includes('/elitea_core/audit/administration') && r.request().method() === 'GET',
  );
  await page.getByRole('tab', { name: 'Spans' }).click();
  const spanResponse = await spanListing;
  expect(spanResponse.status()).toBe(200);
  expect(new URL(spanResponse.url()).searchParams.get('user_id')).toBe(userId);
});

/*
 * The listing is an authorisation boundary, not a UI-visibility one.
 *
 * `GET /admin/auth_users/{mode}` used to have no permission middleware at all:
 * any authenticated session could read every row of `auth_core__user` — id,
 * name, email, last_login, suspended, administration role — while its pylon
 * original gates `get()` on `admin.auth.users`, the same permission it requires
 * for `post()`. A14 gated only the writes, because at the time no deployment
 * had an administration-mode role to hold and gating a read would have meant
 * 403 for everyone; the migrations now seed those roles, so the read is gated.
 *
 * This runs as the MEMBER persona, which holds the default-mode `admin` role
 * and no administration role — so it is exactly the case that used to be
 * allowed. It asserts on the response rather than on the rendered page: an
 * admin SPA that simply does not offer the member a Users link would satisfy a
 * DOM assertion while the endpoint stayed wide open to anything that could
 * issue the request.
 */
adminTest.describe('member persona', () => {
  adminTest.use({ storageState: STORAGE_STATE.member });

  // J33, not J29 — the audit-trail spec claimed J29/J29b/J29c while this
  // branch was open.
  adminTest('J33: a member without admin.auth.users cannot read the global user list', async ({ page }) => {
    const response = await page.request.get(
      `${BASE_URL}/api/v2/admin/auth_users/administration?limit=100&offset=0`,
    );

    expect(response.status(), 'the listing must be refused, not merely hidden').toBe(403);

    // A 403 that still shipped the rows in its body would be the whole defect
    // with a different status code on it.
    const body = await response.text();
    expect(body).not.toContain(SEEDED_ADMIN);
    expect(body).not.toContain(SEEDED_MEMBER);
  });
});

/*
 * ── J39: the cross-project bulk membership invite (issue 247) ────────────────
 *
 * pylon serves this as two admin console pages, `invites_bulkusers` and
 * `invites_bulkprojects`. This platform had no path at all: membership was one
 * address at a time. The route the dialog posts to answers **200 even when
 * pairs fail**, so the only thing that separates "forty invited" from "forty
 * refused" is the per-pair report — which is exactly what this journey reads.
 *
 * ITS OWN PROJECT AND ITS OWN ACCOUNTS, one project per engine
 * (`scripts/e2e-stack.sh`, `e2e-bulkinvite-<engine>`). Not project 1, where
 * every persona lands, and not `e2e-team-active`, whose Admins cell
 * `admin.projects.spec.ts` asserts exactly — a bulk write into either would
 * break a journey that never mentions this one.
 */

/** The bulk-invite fixtures. One project per engine; the invitees are shared. */
function bulkInviteProject(projectName: string): string {
  return projectName === 'chromium' ? 'e2e-bulkinvite-chromium' : 'e2e-bulkinvite-webkit';
}
const BULK_INVITEE_A = 'e2e-bulkinvite-a@autotest.local';
const BULK_INVITEE_B = 'e2e-bulkinvite-b@autotest.local';

/** One pair of the bulk-invite report. */
interface BulkPair {
  readonly user_id: number;
  readonly project_id: number;
  readonly status: string;
  readonly outcome: string;
}

interface BulkReport {
  readonly ok: boolean;
  readonly requested: number;
  readonly added: number;
  readonly skipped: number;
  readonly failed: number;
  readonly results: BulkPair[];
}

/** The project the journey acts on, resolved by NAME rather than by page. */
async function bulkProjectId(page: Page, name: string): Promise<number> {
  const listing = await page.request.get(
    `${BASE_URL}/api/v2/admin/projects/administration?search=${name}&limit=20&offset=0`,
  );
  expect(listing.status(), await listing.text()).toBe(200);
  const rows = ((await listing.json()) as { rows: { id: number; name: string }[] }).rows;
  const project = rows.find((row) => row.name === name);
  expect(project, `the seed must carry the project ${name}`).toBeTruthy();
  return project?.id ?? 0;
}

/** The account ids the batch names, resolved by address. */
async function bulkUserIds(page: Page): Promise<number[]> {
  const ids: number[] = [];
  for (const email of [BULK_INVITEE_A, BULK_INVITEE_B]) {
    const listing = await page.request.get(
      `${BASE_URL}/api/v2/admin/auth_users/administration?limit=20&offset=0&search=${email}`,
    );
    expect(listing.status(), await listing.text()).toBe(200);
    const row = ((await listing.json()) as { rows: { id: number; email: string }[] }).rows.find(
      (entry) => entry.email === email,
    );
    expect(row, `the seed must carry ${email}`).toBeTruthy();
    ids.push(row?.id ?? 0);
  }
  return ids;
}

/** The project's member addresses, read from the SERVER. */
async function projectMemberEmails(page: Page, projectId: number): Promise<string[]> {
  const members = await page.request.get(
    `${BASE_URL}/api/v2/admin/users/administration/${projectId}`,
  );
  expect(members.status(), await members.text()).toBe(200);
  const body = (await members.json()) as { rows?: { email: string }[] } | { email: string }[];
  const rows = Array.isArray(body) ? body : (body.rows ?? []);
  return rows.map((row) => row.email);
}

async function postBulkInvite(
  page: Page,
  payload: { users: number[]; projects: number[]; role: string },
): Promise<BulkReport> {
  const response = await page.request.post(
    `${BASE_URL}/api/v2/admin/invites_bulk/administration`,
    { data: payload },
  );
  expect(response.status(), await response.text()).toBe(200);
  return (await response.json()) as BulkReport;
}

adminTest('J39: a bulk invite writes every pair and the second run reports them as members', async ({ page }, testInfo) => {
  const projectName = bulkInviteProject(testInfo.project.name);
  const projectId = await bulkProjectId(page, projectName);
  const [userA, userB] = await bulkUserIds(page);

  // The batch, through the DIALOG. What the UI does is the claim: the API is
  // covered by the Go integration tests, and a dialog that posts nothing is
  // the #130/#180 defect this suite exists to catch.
  await page.goto(BASE_URL + '/admin/app/users', { waitUntil: 'domcontentloaded' });
  await expect(page.getByText(SEEDED_ADMIN)).toBeVisible({ timeout: 15_000 });
  await page.getByTestId('admin-bulk-invite-open').click();

  const usersField = page.getByTestId('bulk-invite-users').getByRole('combobox');
  await usersField.click();
  await usersField.fill(BULK_INVITEE_A);
  await page.getByRole('option').filter({ hasText: BULK_INVITEE_A }).first().click();
  await usersField.fill(BULK_INVITEE_B);
  await page.getByRole('option').filter({ hasText: BULK_INVITEE_B }).first().click();

  const projectsField = page.getByTestId('bulk-invite-projects').getByRole('combobox');
  await projectsField.click();
  await projectsField.fill(projectName);
  await page.getByRole('option').filter({ hasText: projectName }).first().click();

  await page.getByTestId('bulk-invite-role').getByRole('combobox').click();
  await page.getByRole('option', { name: 'editor' }).click();

  const [inviteResponse] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/admin/invites_bulk/administration') && r.request().method() === 'POST',
    ),
    page.getByTestId('bulk-invite-submit').click(),
  ]);
  // 200 is the route's answer for a partial batch too, so the status alone
  // proves authorisation and nothing else.
  expect(inviteResponse.status(), 'the bulk write must be authorised server-side').toBe(200);

  const report = (await inviteResponse.json()) as BulkReport;
  expect(report.requested, 'two accounts times one project').toBe(2);
  expect(report.failed, JSON.stringify(report.results)).toBe(0);
  // `added` on a fresh stack, `already_member` on a re-run — both mean the
  // account holds the role, and asserting only `added` would make this journey
  // pass once per seeded database.
  for (const pair of report.results) {
    expect(pair.status).toBe('ok');
    expect([userA, userB]).toContain(pair.user_id);
    expect(pair.project_id).toBe(projectId);
  }
  await expect(page.getByTestId('bulk-invite-results')).toBeVisible();

  // THE SERVER'S OWN MEMBER LIST, not the dialog's report: a handler that
  // answered a perfect report and wrote nothing would pass everything above.
  const emails = await projectMemberEmails(page, projectId);
  expect(emails).toContain(BULK_INVITEE_A);
  expect(emails).toContain(BULK_INVITEE_B);

  // Idempotence. Running the same batch again is what an operator who is not
  // sure the first one landed actually does, and pylon's write REPLACED the
  // role set — so a second run there could quietly demote somebody.
  const second = await postBulkInvite(page, {
    users: [userA ?? 0, userB ?? 0],
    projects: [projectId],
    role: 'editor',
  });
  expect(second.ok).toBe(true);
  expect(second.added).toBe(0);
  expect(second.skipped).toBe(2);
  for (const pair of second.results) {
    expect(pair.outcome).toBe('already_member');
  }
});

adminTest('J39b: a bulk invite refuses a personal project and an unknown account, and writes neither', async ({ page }) => {
  const [userA] = await bulkUserIds(page);
  // `project_user_90001`, seeded for admin.projects.spec.ts's personal tab. A
  // personal project is one account's own space; pylon's "add every user"
  // form would have written the whole platform into it.
  const personalProjectId = await bulkProjectId(page, 'project_user_90001');
  const unknownUserId = 90999901;

  const report = await postBulkInvite(page, {
    users: [userA ?? 0, unknownUserId],
    projects: [personalProjectId],
    role: 'editor',
  });

  expect(report.ok, 'both pairs are refusals').toBe(false);
  expect(report.added).toBe(0);
  expect(report.failed).toBe(2);
  const outcomes = report.results.map((pair) => pair.outcome).sort();
  expect(outcomes).toEqual(['personal_project', 'unknown_user']);

  // Nothing was written. A refusal that still wrote the row would report the
  // same summary.
  expect(await projectMemberEmails(page, personalProjectId)).not.toContain(BULK_INVITEE_A);
});

/*
 * NOT COVERED here — deliberately, and each covered elsewhere or tracked:
 *
 *  - the super_admin escalation guard (grant/revoke). It needs a persona
 *    WITHOUT `admin.auth.users.super_admin`, which the E2E stack seeds only one
 *    of; `TestSetAdminRoleGuardsSuperAdminEscalation` in
 *    services/elitea-main/internal/api/v2/admin covers all four of its branches
 *    against a real database.
 *  - delete. It is destructive and the two seeded personas are load-bearing for
 *    every other journey in this suite;
 *    `TestAuthUsersDeleteRemovesTheUser` covers it, re-reading through the
 *    product's own GET handler.
 *  - export. It is REAL now (CSV, not the reference's .xlsx — see
 *    `src/pages/admin/adminUsersCsv.ts`), but the bytes it produces are
 *    asserted where they can be read: `Users.test.tsx` reads the downloaded
 *    Blob, and `adminUsersCsv.test.ts` covers quoting and formula-injection
 *    neutralisation. A browser download in Playwright would re-assert the
 *    same file through a much slower path.
 *  - the activity drawer's ROWS. J34 covers the queries and their scoping,
 *    and `src/pages/admin/UserActivityDrawer.test.tsx` covers the drawer's own
 *    state — but neither asserts what comes back. elitea-main now emits into
 *    `centry.audit_events` itself (its audit middleware records administrative
 *    and security-relevant requests), but it emits for the acting principal on
 *    the surfaces it covers, so a given persona in a given project can still
 *    legitimately have zero rows and an empty table is still a correct answer.
 *  - the nine other admin sections (Projects, Secrets, Roles, …). Not ported
 *    yet — issue #200 lists them. Audit Trail HAS since landed; journey 29
 *    (`admin.audit-trail.spec.ts`) covers it.
 */
