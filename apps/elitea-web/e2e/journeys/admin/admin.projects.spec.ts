/**
 * Journey 31: The admin Projects page reads the real project table, and its
 *             suspend write survives a full reload (JRNY-031)
 *
 * ## Why this journey needs to exist at all
 *
 * Before unit A14, `GET /admin/projects/{mode}` already answered 200 with a
 * `rows` array — it simply ignored `search`, `sort_by`, `sort_order` and
 * `project_type`, sent `owner_id` where the page reads `owner_name`, and
 * carried no `admin_names`, no `status` and no `counts`. So a journey asserting
 * "the page loads" or "a table is present" passes against that. And
 * `PUT /admin/project_suspend/...` had no route at all: its handler existed and
 * nothing could reach it.
 *
 * Every assertion below is therefore against SEEDED ROWS —
 * `scripts/e2e-stack.sh seed` writes three `centry.project` rows plus their
 * owner and admins, and none of those strings exists anywhere in the bundle.
 *
 * The load-bearing assertions are two:
 *   * the team and personal tabs return DIFFERENT sets, from the server;
 *   * the suspend write survives a full page reload, which a handler that
 *     answers 200 and writes nothing (#130, #180) cannot pass.
 */
import { test as adminTest, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

/** Seeded by `scripts/e2e-stack.sh seed` — see its "admin projects fixture" block. */
const SEEDED_TEAM_ACTIVE = 'e2e-team-active';
const SEEDED_TEAM_SUSPENDED = 'e2e-team-suspended';
const SEEDED_PERSONAL = 'project_user_90001';
const SEEDED_OWNER_NAME = 'E2E Project Owner';
const SEEDED_PROJECT_ADMIN = 'E2E Project Admin';

/**
 * The address J31g grants the new project's `admin` role, and the name the
 * listing then renders for it.
 *
 * The MEMBER persona, seeded by `scripts/e2e-stack.sh seed`. It must be an
 * account that already exists: the create handler resolves every
 * `project_admin_email` and refuses the whole request when one is unknown,
 * rolling back every step it had taken.
 */
const PROJECT_ADMIN_EMAIL = 'e2e-member@autotest.local';
const PROJECT_ADMIN_NAME = 'E2E Member';

adminTest('J31: the projects table lists seeded projects with owner, admins and status', async ({ page }) => {
  const response = await page.goto(BASE_URL + '/admin/app/projects', { waitUntil: 'domcontentloaded' });
  expect(response?.status(), 'the admin SPA must serve the projects route, not 404').toBeLessThan(400);

  // From the DATABASE. The listing is gated server-side on
  // `projects.projects.projects.view`; the admin persona holds it via the seed,
  // so a 403 here would mean the gate and the seed have drifted apart.
  await expect(page.getByText(SEEDED_TEAM_ACTIVE)).toBeVisible({ timeout: 20_000 });
  await expect(page.getByText(SEEDED_TEAM_SUSPENDED)).toBeVisible();

  // The empty state and the table are mutually exclusive branches.
  await expect(page.getByText('No projects')).toHaveCount(0);
  await expect(page.getByText('Failed to load projects.')).toHaveCount(0);

  const activeRow = page.getByRole('row').filter({ hasText: SEEDED_TEAM_ACTIVE });

  // `owner_name` and `admin_names` are the two fields the pre-A14 server never
  // sent — it emitted a bare `owner_id`. Both are resolved by the handler from
  // auth_core__user, so neither can come from the bundle.
  await expect(activeRow).toContainText(SEEDED_OWNER_NAME);
  await expect(activeRow).toContainText(SEEDED_PROJECT_ADMIN);

  // The status chip has two DIFFERENT values across the two seeded team
  // projects. A page rendering a constant — the defect the admin Users
  // reference page shipped — cannot produce both.
  await expect(activeRow.getByText('Active', { exact: true })).toBeVisible();
  await expect(
    page.getByRole('row').filter({ hasText: SEEDED_TEAM_SUSPENDED }).getByText('Suspended', { exact: true }),
  ).toBeVisible();

  // Two admins on ONE project. A listing that JOINs the project-role tables
  // rather than aggregating emits the project twice — the row-multiplication
  // defect the admin user listing shipped before A14.
  await expect(page.getByRole('row').filter({ hasText: SEEDED_TEAM_ACTIVE })).toHaveCount(1);

  await checkA11y(page);
});

adminTest('J31b: the two tabs are answered by the server and return different sets', async ({ page }) => {
  await page.goto(BASE_URL + '/admin/app/projects', { waitUntil: 'domcontentloaded' });
  await expect(page.getByText(SEEDED_TEAM_ACTIVE)).toBeVisible({ timeout: 20_000 });

  // The personal project must NOT be on the team tab.
  await expect(page.getByText(SEEDED_PERSONAL)).toHaveCount(0);

  const [personalListing] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/admin/projects/administration') &&
        r.url().includes('project_type=personal') &&
        r.request().method() === 'GET',
    ),
    page.getByRole('tab', { name: /Personal Projects/ }).click(),
  ]);
  expect(personalListing.status(), 'the personal listing must be authorised server-side').toBe(200);

  // The split is the SERVER's: `name LIKE 'project_user_%'`. A client-side tab
  // that filtered nothing would leave the team rows on screen.
  await expect(page.getByText(SEEDED_PERSONAL)).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText(SEEDED_TEAM_ACTIVE)).toHaveCount(0);
});

adminTest('J31c: search is applied by the server, not the browser', async ({ page }) => {
  await page.goto(BASE_URL + '/admin/app/projects', { waitUntil: 'domcontentloaded' });
  await expect(page.getByText(SEEDED_TEAM_ACTIVE)).toBeVisible({ timeout: 20_000 });
  await expect(page.getByText(SEEDED_TEAM_SUSPENDED)).toBeVisible();

  const [searchListing] = await Promise.all([
    page.waitForResponse(
      (r) => r.url().includes('/admin/projects/administration') && r.url().includes('search='),
    ),
    page.getByTestId('admin-projects-search').fill('suspended'),
  ]);
  expect(searchListing.status()).toBe(200);

  await expect(page.getByText(SEEDED_TEAM_ACTIVE)).toHaveCount(0, { timeout: 15_000 });
  await expect(page.getByText(SEEDED_TEAM_SUSPENDED)).toBeVisible();
});

adminTest('J31d: suspending a project reaches the server and survives a full reload', async ({ page }) => {
  await page.goto(BASE_URL + '/admin/app/projects', { waitUntil: 'domcontentloaded' });
  await expect(page.getByText(SEEDED_TEAM_ACTIVE)).toBeVisible({ timeout: 20_000 });

  const row = page.getByRole('row').filter({ hasText: SEEDED_TEAM_ACTIVE });
  // The seed leaves this one ACTIVE; a test that started from "already
  // suspended" would prove nothing about the write.
  await expect(row.getByText('Active', { exact: true })).toBeVisible();

  const suspend = row.getByRole('button', { name: 'Suspend project' });
  await expect(
    suspend,
    'the admin persona holds projects.projects.projects.edit, so the control must be live',
  ).toBeEnabled();

  // The response is what proves the request was AUTHORISED, not merely sent.
  const [suspendResponse] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/admin/project_suspend/administration/') && r.request().method() === 'PUT',
    ),
    suspend.click(),
  ]);
  expect(suspendResponse.status(), 'the suspend write must be authorised server-side').toBe(200);

  // A full reload, not a client-side refetch: this is the assertion a handler
  // that answers 200 and writes nothing cannot pass — and before this unit the
  // route did not exist at all, so the request 404'd.
  await page.reload({ waitUntil: 'domcontentloaded' });
  const afterReload = page.getByRole('row').filter({ hasText: SEEDED_TEAM_ACTIVE });
  await expect(afterReload.getByText('Suspended', { exact: true })).toBeVisible({ timeout: 15_000 });

  // Restore, so this journey leaves the stack as it found it and can be re-run.
  const [unsuspendResponse] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/admin/project_suspend/administration/') && r.request().method() === 'PUT',
    ),
    afterReload.getByRole('button', { name: 'Unsuspend project' }).click(),
  ]);
  expect(unsuspendResponse.status()).toBe(200);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(
    page.getByRole('row').filter({ hasText: SEEDED_TEAM_ACTIVE }).getByText('Active', { exact: true }),
  ).toBeVisible({ timeout: 15_000 });
});

adminTest('J31e: delete cannot fire without the typed confirmation', async ({ page }) => {
  await page.goto(BASE_URL + '/admin/app/projects', { waitUntil: 'domcontentloaded' });
  await expect(page.getByText(SEEDED_TEAM_ACTIVE)).toBeVisible({ timeout: 20_000 });

  // Both provisioning controls are now REAL — this persona's administration
  // role carries `projects.projects.project.create` and `.delete` (shared
  // migration 0069). So is the export (CSV, #587). Nothing on this toolbar is
  // disabled-with-a-reason any more, which is why each is asserted ENABLED: a
  // journey that stopped checking would quietly go on describing controls that
  // are no longer unavailable.
  await expect(page.getByRole('button', { name: 'Create project' })).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Export to CSV' })).toBeEnabled();

  // Delete alone is disabled until a row is selected. That is a state, not a
  // missing feature, and the tooltip says what to do about it.
  const deleteButton = page.getByRole('button', { name: 'Delete projects' });
  await expect(deleteButton).toBeDisabled();

  const row = page.getByRole('row').filter({ hasText: SEEDED_TEAM_ACTIVE });
  await row.getByRole('checkbox').check();
  await expect(deleteButton).toBeEnabled();

  // THE PROPERTY THIS JOURNEY EXISTS FOR. Opening the dialog must not be enough
  // to destroy a tenant: the confirm button stays inert until the word is
  // typed, and the dialog says by name what it would destroy. Asserted against
  // the real server rather than a mock, because it is the last thing standing
  // between a mis-click and `DROP SCHEMA p_<id> CASCADE`.
  await deleteButton.click();
  await expect(page.getByTestId('admin-projects-delete-list')).toContainText(SEEDED_TEAM_ACTIVE);
  await expect(page.getByRole('button', { name: 'Delete permanently' })).toBeDisabled();

  // Backing out leaves the project exactly where it was. Nothing in this
  // journey deletes anything — see the NOT COVERED note at the foot of the file
  // for where the destructive round trip IS exercised.
  await page.getByRole('button', { name: 'Cancel' }).click();
  await expect(page.getByRole('row').filter({ hasText: SEEDED_TEAM_ACTIVE })).toHaveCount(1);
});

adminTest('J31f: the activity drawer reads the audit trail scoped to one project', async ({ page }) => {
  await page.goto(BASE_URL + '/admin/app/projects', { waitUntil: 'domcontentloaded' });
  await expect(page.getByText(SEEDED_TEAM_ACTIVE)).toBeVisible({ timeout: 20_000 });

  const row = page.getByRole('row').filter({ hasText: SEEDED_TEAM_ACTIVE });

  // The drawer reuses the deployment-wide audit endpoints. Every one of its
  // queries must carry this project's id — a drawer that dropped it would show
  // another tenant's traces under this project's name, and the rows would look
  // perfectly plausible.
  // BOTH waiters are registered before the click: the drawer issues its audit
  // and its per-user-activity queries in the same tick, so a waiter registered
  // after the first response arrives has already missed the second.
  const traceListing = page.waitForResponse(
    (r) =>
      r.url().includes('/elitea_core/audit_traces/administration') && r.request().method() === 'GET',
  );
  // `project_user_activity` was an empty-array stub with no route before this
  // unit; it is what feeds the per-member strip.
  const activity = page.waitForResponse((r) =>
    r.url().includes('/elitea_core/project_user_activity/administration'),
  );
  await row.getByRole('button', { name: 'Project activity' }).click();

  const traceResponse = await traceListing;
  expect(traceResponse.status(), 'the audit read must be authorised server-side').toBe(200);
  expect(traceResponse.url()).toContain('project_id=90001');

  const activityResponse = await activity;
  expect(activityResponse.status(), 'per-user activity must be authorised server-side').toBe(200);
  expect(activityResponse.url()).toContain('project_id=90001');

  await expect(page.getByText(`${SEEDED_TEAM_ACTIVE} (ID: 90001)`)).toBeVisible({ timeout: 15_000 });

  // The strip itself holds only unlabelled squares — the "N / M active" caption
  // is its sibling — so the count is asserted on the caption and the membership
  // on the squares' own labels. Both seeded project admins must appear, which
  // is what shows the strip is fed by the project's REAL membership rather than
  // by whatever the last member query happened to return.
  await expect(page.getByText(/\d+ \/ 2 active/)).toBeVisible({ timeout: 15_000 });
  const strip = page.getByTestId('project-user-activity');
  await expect(strip.getByLabel(new RegExp(`^${SEEDED_PROJECT_ADMIN}: `))).toBeVisible();
  await expect(strip.locator('[aria-label]')).toHaveCount(2);
});

/*
 * J31g — THE PROVISIONING ROUND TRIP, IN A BROWSER (issue #377, criterion 7)
 *
 * The other tests in this file assert the state of the two provisioning
 * CONTROLS. This one drives them, because the criterion the issue states is
 * about the list and not about a status code: "the new project appears in the
 * project list", and "the project leaves the list".
 *
 * Why that is not already covered. `TestCreateProjectRouteBuildsTheReferenceTenant`
 * and `TestCreateThenDeleteLeavesTheDeploymentMigratable` drive the same two
 * routes against a real database, and they assert the tenant schema, the vault,
 * the system account and the migration ledger. None of them can say whether the
 * page that calls those routes shows the result: the create mutation refreshes
 * the listing by invalidating a query key, and a key that did not match would
 * leave an operator looking at a list without the project they had just made.
 * That is the #132 shape — the write lands, and the screen does not move.
 *
 * ## Why this does not disturb the rest of the file
 *
 * It works on a project OF ITS OWN, named with the browser project and the
 * clock, so the two engines cannot collide and no run reuses a name. The seeded
 * rows J31 depends on are asserted to be untouched at the end, which is what
 * makes "the delete removed the right row" a statement rather than a hope: a
 * delete that emptied the table would pass an assertion that only looked for
 * the absence of the new name.
 *
 * ## Why it takes its own timeout
 *
 * Creating a project is nine steps — the row, the `p_<id>` tenant schema and
 * its whole migration chain, the permission set, a system account and its
 * token, the vault, the buckets — and deleting it runs them in reverse and ends
 * in `DROP SCHEMA p_<id> CASCADE`. That is minutes on a cold stack, not the 30
 * seconds a journey gets by default.
 */
adminTest('J31g: creating a project puts it in the list, and deleting it takes it out', async ({ page }, testInfo) => {
  adminTest.setTimeout(240_000);
  const name = `e2e-provisioned-${testInfo.project.name}-${Date.now()}`;

  await page.goto(BASE_URL + '/admin/app/projects', { waitUntil: 'domcontentloaded' });
  await expect(page.getByText(SEEDED_TEAM_ACTIVE)).toBeVisible({ timeout: 20_000 });
  // Nothing of this name exists yet, so the assertion after the create cannot
  // be satisfied by a row that was already on screen.
  await expect(page.getByRole('row').filter({ hasText: name })).toHaveCount(0);

  /* ── create ─────────────────────────────────────────────────────────── */
  await page.getByRole('button', { name: 'Create project' }).click();
  await page.getByLabel('Project name').fill(name);
  // The dialog sends `project_admin_email` (criterion 2), so a project made in
  // the user interface always has a human administrator. The address must
  // already have an account: the server resolves each one and refuses the whole
  // request when it cannot, rolling every step back.
  const adminEmails = page.getByLabel('Project admin email(s)');
  await adminEmails.fill(PROJECT_ADMIN_EMAIL);
  await adminEmails.press('Enter');

  const [created] = await Promise.all([
    page.waitForResponse(
      (r) => r.url().includes('/projects/project/administration') && r.request().method() === 'POST',
      { timeout: 180_000 },
    ),
    page.getByRole('button', { name: 'Create', exact: true }).click(),
  ]);
  // Read for the MESSAGE, not as the acceptance: a provisioning refusal names
  // the step it stopped at, and a journey that only reported "the row is not on
  // screen" would hide it.
  expect(
    created.status(),
    `provisioning must complete: ${(await created.text()).slice(0, 500)}`,
  ).toBe(201);

  // THE CRITERION. A full reload, not the mutation's own cache invalidation, so
  // this cannot pass on an optimistic row that the server never stored.
  await page.reload({ waitUntil: 'domcontentloaded' });
  const provisioned = page.getByRole('row').filter({ hasText: name });
  await expect(provisioned).toHaveCount(1, { timeout: 30_000 });
  // The administrator the form asked for is on the row. `admin_names` is
  // resolved by the listing from the project-role tables, so this is what says
  // the address in the dialog became a real membership rather than a body field
  // the server ignored.
  await expect(provisioned).toContainText(PROJECT_ADMIN_NAME);
  await expect(provisioned.getByText('Active', { exact: true })).toBeVisible();

  /* ── delete ─────────────────────────────────────────────────────────── */
  await provisioned.getByRole('checkbox').check();
  const deleteButton = page.getByRole('button', { name: 'Delete projects' });
  await expect(deleteButton).toBeEnabled();
  await deleteButton.click();

  // The dialog names what it is about to destroy, and stays inert until the
  // word is typed — J31e asserts that property on a seeded row and backs out;
  // this is the one place the armed button is actually pressed.
  await expect(page.getByTestId('admin-projects-delete-list')).toContainText(name);
  const confirm = page.getByRole('button', { name: 'Delete permanently' });
  await expect(confirm).toBeDisabled();
  await page.getByLabel('Type DELETE to confirm').fill('DELETE');
  await expect(confirm).toBeEnabled();

  const [deleted] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/projects/project/administration/') && r.request().method() === 'DELETE',
      { timeout: 180_000 },
    ),
    confirm.click(),
  ]);
  expect(
    deleted.status(),
    `deprovisioning must complete: ${(await deleted.text()).slice(0, 500)}`,
  ).toBe(200);

  // THE OTHER HALF OF THE CRITERION, after a full reload again.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByText(SEEDED_TEAM_ACTIVE)).toBeVisible({ timeout: 30_000 });
  await expect(page.getByRole('row').filter({ hasText: name })).toHaveCount(0);
  // …and the seeded rows are exactly where they were. A delete that took the
  // wrong project, or the whole table, passes the line above and fails these.
  await expect(page.getByRole('row').filter({ hasText: SEEDED_TEAM_ACTIVE })).toHaveCount(1);
  await expect(page.getByRole('row').filter({ hasText: SEEDED_TEAM_SUSPENDED })).toHaveCount(1);
  await expect(page.getByText('No projects')).toHaveCount(0);
});

/*
 * NOT COVERED here — deliberately, and each covered elsewhere or stated:
 *
 *  - the provisioning PIPELINE's internals. The round trips themselves ARE
 *    driven through the browser now, by J31g (issue #377, criterion 7): it
 *    creates a project OF ITS OWN, asserts it enters the list, deletes it, and
 *    asserts it leaves while the seeded rows stay. What J31g does not read is
 *    what the nine steps wrote — the tenant schema, the vault, the system
 *    account, the migration ledger. Those need SQL, and
 *    `TestCreateProjectRouteBuildsTheReferenceTenant` and
 *    `TestCreateThenDeleteLeavesTheDeploymentMigratable` in
 *    services/elitea-main/internal/api/v2/projects assert them directly.
 *    J31e still covers the part that is only observable in a browser — that the
 *    irreversible control cannot fire on a click alone — and it does so against
 *    a SEEDED row, which J31g must not touch.
 *  - the member dialog's WRITES. They reach real handlers
 *    (`POST`/`PUT /admin/users/administration/{projectID}`) and are covered by
 *    `TestUsersWriteVerbsPersistProjectMembership` in
 *    services/elitea-main/internal/api/v2/eliteacore, which re-reads through the
 *    product's own GET, and by `src/pages/admin/Projects.test.tsx`, which
 *    asserts the exact bodies. They are not exercised here because the seeded
 *    project's membership is load-bearing for J31's `admin_names` assertion,
 *    and a journey that added a member would change what J31 asserts.
 *  - the negative authorisation case (a persona WITHOUT
 *    `projects.projects.projects.edit`). The E2E stack seeds one administration
 *    persona; `TestProjectSuspendIsRefusedWithoutTheEditPermission` and
 *    `TestProjectsListingIsRefusedWithoutTheViewPermission` cover both
 *    directions against a real database.
 *  - the export's CONTENTS. The control is real (CSV, not the reference's
 *    .xlsx — see `src/pages/admin/adminCsv.ts`), and J31e asserts it is
 *    enabled, but the bytes are asserted where they can be read:
 *    `Projects.test.tsx` reads the downloaded Blob, and `adminCsv.test.ts`
 *    covers quoting and formula-injection neutralisation. A browser download
 *    in Playwright would re-assert the same file through a much slower path.
 */
