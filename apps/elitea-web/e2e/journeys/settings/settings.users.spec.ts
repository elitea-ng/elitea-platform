/**
 * Journey 22: Settings: invite user, change role (JRNY-022)
 *
 * Spec §8.5 acceptance (from parity/manifest/users.json JRNY-022).
 * Acceptance: the member list reflects both changes;
 * the invite modal is reachable directly by URL.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * WHAT THIS FILE CAN AND CANNOT PROVE TODAY
 *
 * Every `test.skip()` / early `return` / `.catch(() => false)` that used to
 * live here has been removed. Nothing below is guarded: each assertion either
 * holds against the real seeded stack or fails the run.
 *
 * HISTORY — this header used to say the journey's headline acceptance ("the
 * member list reflects both changes") was unassertable, for two reasons. Both
 * are gone:
 *
 *  1. D-USR-1/D-USR-2 (the double-unwraps) were fixed, so the grid draws rows
 *     and the Roles dropdown offers the seeded roles. J22c/J22d cover them.
 *  2. The write path was a live no-op: the router mounted POST/PUT/DELETE of
 *     /admin/users/{mode}/{projectID} on the SAME listing handler, which does
 *     no method branching at all — every verb ran the SELECT and answered 200
 *     with the member list, so Invite/Edit/Delete reported success and wrote
 *     nothing. (The header also blamed `oapiserver/admin.go`'s
 *     `{user_id,role_id}` decoding. That was wrong: the generated server in
 *     that package is never mounted, so the chi router never reaches it. #130's
 *     original diagnosis was corrected in its own comments.) The three verbs
 *     now have real handlers — services/elitea-main/internal/api/v2/eliteacore/
 *     users_write.go — speaking pylon's contract.
 *
 * So J22e/J22f/J22g below finally assert the headline acceptance. Note what
 * they do NOT do: assert on the response status. 200 is exactly what the broken
 * handler already returned, so a status assertion proves nothing here. Each one
 * performs the write through the UI and then RE-READS it after a full page
 * reload — and, for the invite, straight off the API as well.
 *
 * J22h extends the same discipline to the BATCH edit (#188). Everything above
 * drives one row at a time, and the batch path is a different one on both
 * sides: the client joins the selected ids into a single `userId` string
 * (`entities/user/model/useEditUser.ts`'s `useBatchEditUsers`), the dialog is
 * reached through the HEADER control rather than a row's, and it opens with NO
 * role selected instead of the selected user's. Up to J22h that path was
 * covered only at the API level, by `users_write_postgres_integration_test.go`'s
 * "PUT accepts the comma-joined batch id form" — which says nothing about
 * whether a browser ever produces that form.
 *
 * NOT covered: `administration` mode. It addresses the global scope rather than
 * a project's membership and answers 501 by design; no UI reaches it.
 *
 * MUTATION RECORD — every assertion in J22a/J22b was verified to fail when the
 * code it covers is broken. The E2E stack serves a PREBUILT elitea-web image,
 * so mutations were applied by rewriting the served chunk in-browser
 * (`page.route` text substitution) rather than by editing src — same code,
 * same expression, isolated to one browser context:
 *   J22a  `inviteUsers === '1'` → `=== '9'`            → dialog never opens  ✗
 *   J22a  gate removed, effect always fires            → dialog on bare URL  ✗
 *   J22a  invite IconButton `onClick` → no-op          → click opens nothing ✗
 *   J22b  `validateEmails` always returns valid        → no error text       ✗
 *   J22b  Invite `disabled={…}` → `false`              → button not disabled ✗
 * J22c/J22d needed no mutation when they were written: they failed on the
 * unmutated build, and pass now that the two unwraps are fixed.
 *
 * J22e/J22f/J22g were mutation-verified against the SERVER, which is where
 * their subject lives. Reverting internal/api/router.go's three write mounts
 * back to `coreHandler.Users` (i.e. restoring the #130 defect) and rebuilding
 * + recreating the elitea-main image (verified by image digest, since
 * `e2e-stack.sh up` reuses an existing tag) fails all three — and every one of
 * them fails on the RE-READ, never on a status:
 *   J22e  invite   → "POST returned success but …@autotest.local is not a member"
 *                    (the success toast still appeared)
 *   J22f  edit     → roles after edit = ['viewer'], want ['editor']
 *   J22g  remove   → the removed address is still in the member list
 * The two client-side fixes in this change were measured the same way: before
 * `UsersPageContent`'s stray always-open `EditUserRolesDialog` was removed,
 * J22f could not find the Edit control at all (the modal marks the page
 * `aria-hidden`); before `UsersTable`'s actions cell stopped the click from
 * reaching the DataGrid row, clicking the row's pencil deselected the row and
 * the dialog never opened.
 *
 * J22h was measured on both sides of the batch path:
 *   SERVER — `UsersUpdate`'s `for _, userID := range userIDs` → `userIDs[:1]`
 *            (users_write.go), rebuilt and the container recreated (image
 *            digest verified to change, since `e2e-stack.sh up` reuses the
 *            `:e2e` tag). J22h fails with
 *            `autotest_batch-b@autotest.local after the batch edit: ["viewer"]
 *            != ["admin"]` — on the RE-READ, never on a status: the mutated
 *            handler still answered 200 and the success toast still appeared.
 *   CLIENT — a `page.route` rewrite of the outgoing PUT body's `userId` to its
 *            first comma-separated element (i.e. a client that stopped joining
 *            the selection) produces the SAME observable: member A is updated,
 *            member B is left on `viewer`.
 * Two more probes established that the batch-specific assertions are not
 * satisfied by the single-row path they are meant to distinguish from:
 *   - with ONE row selected, the same dialog opens with `viewer` TICKED, so
 *     J22h's "no role is ticked" check really is the batch/single
 *     discriminator and not something every dialog passes;
 *   - with two rows selected the page carries THREE controls labelled
 *     "Edit role" (the header's plus one per selected row), which is why J22h
 *     locates the header's structurally instead of by `.first()`.
 * ─────────────────────────────────────────────────────────────────────────
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { AUTOTEST_PREFIX, API_BASE, DEFAULT_PROJECT_ID } from '../../fixtures/api';

/** Roles seeded into project 1 by scripts/e2e-stack.sh (`admin`, `editor`, `viewer`). */
const SEEDED_ROLES = ['admin', 'editor', 'viewer'] as const;

/** Members seeded into project 1, with the project role each one carries. */
const SEEDED_MEMBERS = [
  { email: 'e2e-admin@autotest.local', name: 'E2E Admin', role: 'admin' },
  { email: 'e2e-member@autotest.local', name: 'E2E Member', role: 'editor' },
] as const;

const INVITE_EMAIL = `${AUTOTEST_PREFIX}invite-usr@autotest.local`;

/** The invite dialog carries no data-testid; its title is the only landmark. */
const inviteDialog = (page: import('@playwright/test').Page) =>
  page.getByRole('dialog').filter({ hasText: 'Invite users' });

test('J22a: invite dialog is reachable by URL and only by the flag', async ({
  page,
}) => {
  test.setTimeout(60_000);

  // QP-002: `?inviteUsers=1` must open the dialog. No fallback path — if the
  // deep link is broken this test is the thing that says so.
  await page.goto(BASE_URL + '/app/settings/users?inviteUsers=1');

  const dlg = inviteDialog(page);
  await expect(dlg).toBeVisible({ timeout: 15_000 });

  // NOTE: do NOT assert here that the URL no longer carries `inviteUsers=1`.
  // It looks like proof that the `navigate({search:{}, replace:true})` half of
  // the effect (src/pages/settings/Users.tsx:222) ran, but it is not: the route's
  // own `validateSearch`/`pickParams` normalisation rewrites the whole search
  // string (to `inviteUsers="0"` plus every other route default) even when that
  // navigate call is deleted. Mutation-verified — removing the navigate leaves
  // this passing, so it is a tautology, not an assertion.

  // Controls a heading-only stub cannot produce: a required Emails field and a
  // Roles combobox, both scoped inside the dialog.
  await expect(dlg.getByRole('textbox', { name: /emails/i })).toBeVisible();
  await expect(dlg.getByRole('combobox', { name: /roles/i })).toBeVisible();
  await expect(dlg.getByRole('button', { name: /^invite$/i })).toBeVisible();

  // Cancel is wired to onClose (UsersPageContent.tsx:352) — the dialog must go.
  await dlg.getByRole('button', { name: /^cancel$/i }).click();
  await expect(dlg).toHaveCount(0);

  // The dialog must be FLAG-driven, not always-on: the same route without the
  // query parameter renders the page with no dialog at all. This is the half of
  // QP-002 that a permanently-mounted modal (or a stub that always shows one)
  // would otherwise satisfy for free.
  await page.goto(BASE_URL + '/app/settings/users');
  await expect(page.getByPlaceholder('Search users…')).toBeVisible({ timeout: 15_000 });
  await expect(dlg).toHaveCount(0);

  // Header controls gated on real RBAC grants (canView → search, canCreate →
  // invite). The e2e-member persona holds `configuration.users.users.{view,create}`
  // via its project `editor` role, so both must render; a permission regression
  // fails here. Clicking Invite must open the same dialog.
  const inviteControl = page.locator('button[title="Invite users"]');
  await expect(inviteControl).toBeVisible();
  await inviteControl.click();
  await expect(dlg).toBeVisible();

  await checkA11y(page);
});

test('J22b: invite form enforces its validity contract', async ({ page }) => {
  test.setTimeout(60_000);

  await page.goto(BASE_URL + '/app/settings/users?inviteUsers=1');
  const dlg = inviteDialog(page);
  await expect(dlg).toBeVisible({ timeout: 15_000 });

  const emails = dlg.getByRole('textbox', { name: /emails/i }).first();
  const inviteBtn = dlg.getByRole('button', { name: /^invite$/i });

  // State 1 — nothing entered: Invite is disabled.
  await expect(inviteBtn).toBeDisabled();

  // State 2 — a malformed address surfaces the exact validator message
  // (InviteUserDialog.tsx:48) and keeps Invite disabled.
  await emails.fill('not-an-email');
  await emails.blur();
  await expect(dlg.getByText('Invalid email: not-an-email', { exact: true })).toBeVisible();
  await expect(inviteBtn).toBeDisabled();

  // State 3 — a valid address clears the error and is retained verbatim, but
  // Invite stays disabled because no role has been chosen yet. This is the
  // `selectedRoles.length === 0` arm of the gate at InviteUserDialog.tsx:174.
  await emails.fill(INVITE_EMAIL);
  await emails.blur();
  await expect(dlg.getByText(/^Invalid email:/)).toHaveCount(0);
  await expect(emails).toHaveValue(INVITE_EMAIL);
  await expect(inviteBtn).toBeDisabled();
});

/**
 * D-USR-2 regression guard.
 *
 * `useRoleList` returns the live body `[{"id":"1","name":"admin"}, …]` (verified
 * over the wire against the seeded stack), and src/pages/settings/Users.tsx used
 * to read `roleListQuery.data?.data?.data?.rows` — one unwrap too many, against
 * a body that is a bare array and has no `rows` key at all. `rolesOptions` was
 * therefore always `[]`, and SingleSelect fell back to its disabled
 * "No options" item (SingleSelect.tsx:164-170), which also made the Invite
 * button permanently disabled.
 *
 * This test is the only thing standing between "the roles dropdown is empty"
 * and "the roles dropdown is empty and nobody noticed".
 */
test('J22c: roles dropdown is populated from the seeded server roles', async ({ page }) => {
  test.setTimeout(60_000);

  await page.goto(BASE_URL + '/app/settings/users?inviteUsers=1');
  const dlg = inviteDialog(page);
  await expect(dlg).toBeVisible({ timeout: 15_000 });

  await dlg.getByRole('combobox', { name: /roles/i }).click();

  // The dropdown portals to the body, so options are located on `page`.
  // `select-option-<value>` is stamped by SingleSelectDropdown.tsx:33 with the
  // value that came off the wire — a stub cannot mint these three ids.
  for (const role of SEEDED_ROLES) {
    await expect(page.locator(`[data-testid="select-option-${role}"]`)).toBeVisible();
  }
  await expect(page.getByRole('option', { name: /no options/i })).toHaveCount(0);

  // Choosing a role is the last gate on the Invite button.
  await page.locator('[data-testid="select-option-editor"]').click();
  await dlg.getByRole('textbox', { name: /emails/i }).first().fill(INVITE_EMAIL);
  await expect(dlg.getByRole('button', { name: /^invite$/i })).toBeEnabled();
});

/**
 * D-USR-1 regression guard.
 *
 * GET /api/v2/admin/users/default/1 returns
 * `{"rows":[{email:"e2e-admin@autotest.local",…,roles:["admin"]}, …],"total":2}`
 * (asserted below). src/pages/settings/Users.tsx used to read
 * `userListQuery.data?.data?.data`, one unwrap too many, so `rows` was always
 * `[]` and UsersTable rendered its "No users" branch (UsersTable.tsx:245-266)
 * while the pagination footer, gated on `total > 0`, never mounted at all.
 *
 * The response assertion and the DOM assertions are deliberately in the same
 * test: together they localise the defect to the client mapping.
 */
test('J22d: member list renders the seeded project members', async ({ page }) => {
  test.setTimeout(60_000);

  const userListResponse = page.waitForResponse(
    (r) => r.url().includes('/admin/users/default/') && r.request().method() === 'GET',
    { timeout: 20_000 },
  );
  await page.goto(BASE_URL + '/app/settings/users');

  // The server half of the contract.
  const body = (await (await userListResponse).json()) as {
    rows: { email: string; roles: string[] }[];
    total: number;
  };
  expect(body.total).toBeGreaterThanOrEqual(2);
  for (const member of SEEDED_MEMBERS) {
    expect(body.rows.map((r) => r.email)).toContain(member.email);
  }

  // The client half. `No users` must NOT be what a project with two seeded
  // members shows.
  await expect(page.getByText('No users')).toHaveCount(0);

  const grid = page.getByRole('grid');
  await expect(grid).toBeVisible({ timeout: 15_000 });

  for (const member of SEEDED_MEMBERS) {
    const row = grid.getByRole('row').filter({ hasText: member.email });
    await expect(row).toHaveCount(1);
    // Role comes from the JOIN over auth_core__project_user_role /
    // auth_core__project_role — no static markup can produce this pairing.
    await expect(row.getByRole('gridcell', { name: member.role, exact: true })).toBeVisible();
    await expect(row.getByRole('gridcell', { name: member.name, exact: true })).toBeVisible();
  }

  // Pagination footer prints the SERVER total (Users.tsx:322, UsersPageContent.tsx:284)
  // and only renders when `total > 0`.
  await expect(page.getByText(/^Showing 1–\d+ of \d+$/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Previous page' })).toBeDisabled();

  // Search is wired to the fetched rows, not to a static list: filtering to the
  // admin persona must drop the member row and keep the admin row.
  await page.getByPlaceholder('Search users…').fill('e2e-admin');
  await expect(
    grid.getByRole('row').filter({ hasText: 'e2e-member@autotest.local' }),
  ).toHaveCount(0);
  await expect(
    grid.getByRole('row').filter({ hasText: 'e2e-admin@autotest.local' }),
  ).toHaveCount(1);

  await checkA11y(page);
});

/* ─── #130: the write path ──────────────────────────────────────────────────
 *
 * These three tests are the reason #130 is closable. Read the note in the file
 * header before touching them: the broken build answered 200 to POST, PUT and
 * DELETE alike, so `expect(response.status()).toBe(200)` is satisfied by the
 * bug. Only a RE-READ discriminates, and the re-read here is a full
 * `page.goto` — not a soft refetch — so nothing can be served out of the
 * client's react-query cache.
 *
 * They operate on addresses they create themselves and remove again, never on
 * the seeded personas: e2e-admin/e2e-member's roles are load-bearing for every
 * other spec's RBAC, and the settings-users visual baseline is a screenshot of
 * exactly this grid.
 *
 * ONE ENGINE PER STACK. Every test in this group asserts an exact membership
 * count (`before.length ± 1`, `before.length`) against a project the whole
 * suite shares, so it needs the stack to itself. That is what CI gives it:
 * ci-web-e2e.yml runs `--project=${{ matrix.engine }}` in a matrix, one engine
 * per job, each with its own stack. A local `npm run e2e` does NOT — it runs
 * chromium and webkit against the same containers, and the two engines' copies
 * of these tests then race over the same rows. Run them one engine at a time
 * (`npx playwright test --project=chromium …`) when reproducing locally;
 * a failure with a member the test just swept still present is that race, not
 * a defect in the write path.
 */

/** The address J22e invites and J22g removes. Distinct from J22b's, which is never sent. */
const WRITE_PATH_EMAIL = `${AUTOTEST_PREFIX}invite-persist@autotest.local`;

const usersApiUrl = `${API_BASE}/admin/users/default/${DEFAULT_PROJECT_ID}?limit=200&offset=0`;

interface ApiMember {
  readonly id: string;
  readonly email: string;
  readonly roles: readonly string[];
}

/**
 * The membership as the SERVER has it. The context passed in must be an
 * authenticated one — `page.request` (which shares the browser context's
 * cookies) or a context built with `STORAGE_STATE.member`. The bare `request`
 * fixture is anonymous and 401s.
 */
async function apiMembers(
  api: import('@playwright/test').APIRequestContext,
): Promise<ApiMember[]> {
  const resp = await api.get(usersApiUrl);
  if (!resp.ok()) {
    throw new Error(`GET ${usersApiUrl} -> ${resp.status()}: ${(await resp.text()).slice(0, 300)}`);
  }
  const body = (await resp.json()) as { rows?: ApiMember[] };
  return body.rows ?? [];
}

/**
 * Removes the given test addresses from the project, skipping any that are not
 * members. Idempotent.
 *
 * One DELETE per address on purpose, even though the endpoint also accepts the
 * comma-joined `?id[]=3,4` batch form. A sweep that leaned on the batch form
 * would share a failure mode with the thing J22h exists to test: if the server
 * ever stopped splitting a joined id list, the sweep would remove one row of
 * two and the leftover would land in the next run's grid (and in the
 * `settings-users` visual baseline) as a mystery.
 */
async function removeMembersByEmail(
  api: import('@playwright/test').APIRequestContext,
  emails: readonly string[],
): Promise<void> {
  const members = await apiMembers(api);
  for (const email of emails) {
    const member = members.find((row) => row.email === email);
    if (!member) continue;
    const resp = await api.delete(
      `${API_BASE}/admin/users/default/${DEFAULT_PROJECT_ID}?${new URLSearchParams({ 'id[]': member.id }).toString()}`,
    );
    if (!resp.ok()) {
      throw new Error(
        `cleanup DELETE ${email} -> ${resp.status()}: ${(await resp.text()).slice(0, 300)}`,
      );
    }
  }
}

/**
 * Adds addresses to the project through the product's own invite endpoint —
 * the one `InviteUserDialog` → `useInviteUsers` POSTs to, with the same
 * `{emails, roles}` body. Setup for a test whose subject is the EDIT path, so
 * it does not spend a second UI pass on an invite J22e already drives through
 * the browser; but it stays on the product's creation path rather than
 * inserting `auth_core__project_user_role` rows behind the API's back, so a
 * membership this helper reports is a membership the app can produce.
 */
async function inviteMembers(
  api: import('@playwright/test').APIRequestContext,
  emails: readonly string[],
  roles: readonly string[],
): Promise<void> {
  const url = `${API_BASE}/admin/users/default/${DEFAULT_PROJECT_ID}`;
  const resp = await api.post(url, { data: { emails: [...emails], roles: [...roles] } });
  if (!resp.ok()) {
    throw new Error(`setup POST ${url} -> ${resp.status()}: ${(await resp.text()).slice(0, 300)}`);
  }
}

/**
 * Opens the roles dropdown of the invite dialog and picks one role by value.
 *
 * Do NOT press Escape afterwards to "close the dropdown". `SingleSelect` is a
 * single-select MUI `Select`, so choosing an option closes its menu already —
 * the Escape then reaches the Dialog, closes IT, and `InviteUserDialog`'s
 * `if (!open) …` effect resets `inputText`/`selectedRoles`. The visible symptom
 * is an Invite button that is present and disabled with everything filled in,
 * which reads exactly like a broken validity gate.
 */
async function pickInviteRole(page: import('@playwright/test').Page, role: string): Promise<void> {
  const dlg = inviteDialog(page);
  await dlg.getByRole('combobox', { name: /roles/i }).click();
  await page.locator(`[data-testid="select-option-${role}"]`).click();
  await expect(page.locator(`[data-testid="select-option-${role}"]`)).toHaveCount(0);
}

/**
 * Selects exactly one grid row by the email it shows.
 *
 * The tick is retried rather than clicked once. `UsersTable` drives the
 * DataGrid's `rowSelectionModel` entirely from the parent's `selectedUsers`,
 * so the checkbox only turns on once React has round-tripped the click through
 * `handleSelectionModelChange` → `onSelectRow` → `setSelectedUsers`; a
 * re-render arriving inside that window swallows the click and a bare
 * `check()` throws "Clicking the checkbox did not change its state". Measured
 * at roughly one run in eight on this stack, and it reads like a defect in the
 * write path rather than in the selection wiring, which is the whole problem.
 *
 * `check()` is a no-op on an already-ticked box, so retrying cannot
 * over-select, and the explicit `toBeChecked` is what makes the retry a
 * verified end state instead of a hopeful click.
 */
async function selectRowByEmail(page: import('@playwright/test').Page, email: string) {
  const grid = page.getByRole('grid');
  const row = grid.getByRole('row').filter({ hasText: email });
  await expect(row).toHaveCount(1, { timeout: 15_000 });
  const checkbox = row.getByRole('checkbox');
  await expect(async () => {
    await checkbox.check({ timeout: 5_000 });
    await expect(checkbox).toBeChecked({ timeout: 2_000 });
  }).toPass({ timeout: 20_000 });
  return row;
}

test.describe('#130 write path', () => {
  test.describe.configure({ mode: 'serial' });

  // `browser.newContext()` with no storageState is ANONYMOUS — a sweep run
  // through it would 401 and remove nothing while reporting success.
  test.beforeAll(async ({ browser }) => {
    // A previous aborted run may have left the address in the project; the
    // grid's row count and J22e's "was not a member before" precondition both
    // depend on starting clean.
    const context = await browser.newContext({ storageState: STORAGE_STATE.member });
    await removeMembersByEmail(context.request, [WRITE_PATH_EMAIL]);
    await context.close();
  });

  test.afterAll(async ({ browser }) => {
    const context = await browser.newContext({ storageState: STORAGE_STATE.member });
    await removeMembersByEmail(context.request, [WRITE_PATH_EMAIL]);
    // Leaving this row behind would change the `settings-users` visual
    // baseline, so the sweep is asserted, not hoped for.
    expect((await apiMembers(context.request)).map((row) => row.email)).not.toContain(
      WRITE_PATH_EMAIL,
    );
    await context.close();
  });

  test('J22e: an invite is actually written and survives a full reload', async ({ page }) => {
    test.setTimeout(90_000);

    await page.goto(BASE_URL + '/app/settings/users');
    await expect(page.getByRole('grid')).toBeVisible({ timeout: 15_000 });
    const before = await apiMembers(page.request);
    expect(before.map((row) => row.email)).not.toContain(WRITE_PATH_EMAIL);

    await page.goto(BASE_URL + '/app/settings/users?inviteUsers=1');
    const dlg = inviteDialog(page);
    await expect(dlg).toBeVisible({ timeout: 15_000 });
    await dlg.getByRole('textbox', { name: /emails/i }).first().fill(WRITE_PATH_EMAIL);
    await pickInviteRole(page, 'viewer');

    const inviteBtn = dlg.getByRole('button', { name: /^invite$/i });
    await expect(inviteBtn).toBeEnabled();
    await inviteBtn.click();

    // The success toast is NOT evidence — the broken build showed it too,
    // because the no-op handler's 200 drove `onSuccess`. It is only waited on
    // so the request has certainly been issued.
    // The toast names what actually happened (ADR-0024 WP7): "invited by
    // e-mail" only when the deployment sends mail, "added" otherwise. The
    // E2E stack configures no SMTP, so it sees the second; a stack with a
    // relay sees the first. Either proves the write landed.
    await expect(
      page.getByText(/The user has been (added\. No invitation e-mail was sent\.|invited by e-mail)/),
    ).toBeVisible({ timeout: 15_000 });

    // RE-READ #1: straight off the server, no client code in the path.
    const after = await apiMembers(page.request);
    const written = after.find((row) => row.email === WRITE_PATH_EMAIL);
    expect(written, `POST returned success but ${WRITE_PATH_EMAIL} is not a member`).toBeTruthy();
    expect(written?.roles).toEqual(['viewer']);
    expect(after).toHaveLength(before.length + 1);

    // RE-READ #2: a fresh page load, so the row can only come from the server.
    await page.goto(BASE_URL + '/app/settings/users');
    const row = page.getByRole('grid').getByRole('row').filter({ hasText: WRITE_PATH_EMAIL });
    await expect(row).toHaveCount(1, { timeout: 15_000 });
    await expect(row.getByRole('gridcell', { name: 'viewer', exact: true })).toBeVisible();
  });

  test('J22f: an edited role REPLACES the old one and survives a full reload', async ({ page }) => {
    test.setTimeout(90_000);

    await page.goto(BASE_URL + '/app/settings/users');
    const target = await selectRowByEmail(page, WRITE_PATH_EMAIL);

    // Selecting a row must not pop a dialog by itself. `UsersPageContent` used
    // to mount a third `EditUserRolesDialog` with
    // `open={Boolean(singleAction?.edit || batchAction?.edit)}` and a no-op
    // `onClose`, so one tick of a checkbox threw an undismissable modal over
    // the page — and, since a modal marks everything behind it `aria-hidden`,
    // the Edit and Delete controls below stopped existing for assistive tech.
    await expect(page.getByRole('dialog')).toHaveCount(0);

    // The per-row edit control renders only for a SELECTED row
    // (UsersTable.tsx's actions column: `editProp?.userIds?.includes(rowId)`),
    // so this click also proves the selection wiring. Scoped to the row rather
    // than `.first()` on the page: the header carries an identically-labelled
    // control, and `.first()` silently picks whichever comes first in the DOM.
    await target.getByRole('button', { name: 'Edit role' }).click();
    const editDialog = page.getByRole('dialog').filter({ hasText: 'Edit roles' });
    await expect(editDialog).toBeVisible();

    // Multi-select: check `editor`, uncheck the `viewer` J22e granted. Save is
    // `disabled={!selectedRoleIds.length || !hasChanged}`, so both clicks matter.
    //
    // This block used to carry a KNOWN FLAKE note: ~1 run in 15, the
    // `expect(save).toBeEnabled()` below timed out with Save still disabled,
    // because `EditUserRolesDialog`'s reset effect keyed on the `originalRoles`
    // ARRAY IDENTITY and every parent render minted a fresh array — so any
    // re-render landing while the dialog was open (a react-query refetch, the
    // toast timer) discarded the ticks above. The effect now keys on the role
    // VALUES, and `useUsersActions`/`useEditUser` no longer rebuild their
    // callbacks each render. Unit cover: `EditUserRolesDialog.test.tsx`. If
    // this assertion starts timing out again, that is a real regression — do
    // not paper over it by retrying the ticks.
    await editDialog.getByRole('combobox').click();
    await page.getByRole('option', { name: 'editor' }).click();
    await page.getByRole('option', { name: 'viewer' }).click();
    await page.keyboard.press('Escape');

    const save = editDialog.getByRole('button', { name: /^save$/i });
    await expect(save).toBeEnabled();
    await save.click();
    await expect(page.getByText('The user has been edited successfully')).toBeVisible({ timeout: 15_000 });

    // RE-READ #1 — and it has to be a REPLACEMENT: a handler that merely
    // inserted the new row would report ['editor','viewer'] here.
    const roles = (await apiMembers(page.request)).find((m) => m.email === WRITE_PATH_EMAIL)?.roles;
    expect(roles, `PUT returned success but ${WRITE_PATH_EMAIL} has no roles`).toBeTruthy();
    expect([...(roles ?? [])].sort()).toEqual(['editor']);

    // RE-READ #2 — the grid after a full reload.
    await page.goto(BASE_URL + '/app/settings/users');
    const reloaded = page.getByRole('grid').getByRole('row').filter({ hasText: WRITE_PATH_EMAIL });
    await expect(reloaded).toHaveCount(1, { timeout: 15_000 });
    await expect(reloaded.getByRole('gridcell', { name: 'editor', exact: true })).toBeVisible();
    await expect(reloaded.getByRole('gridcell', { name: 'viewer', exact: true })).toHaveCount(0);
  });

  test('J22g: removing a member actually removes them and survives a full reload', async ({ page }) => {
    test.setTimeout(90_000);

    await page.goto(BASE_URL + '/app/settings/users');
    const before = await apiMembers(page.request);
    expect(before.map((m) => m.email)).toContain(WRITE_PATH_EMAIL);

    await selectRowByEmail(page, WRITE_PATH_EMAIL);
    // Delete lives only in the header (UsersPageHeader.tsx), gated on
    // `selectedUsers.length >= 1` — there is no per-row delete control.
    await page.getByRole('button', { name: 'Delete user' }).first().click();
    const confirm = page.getByRole('dialog').filter({ hasText: 'Delete user' });
    await expect(confirm).toBeVisible();
    await confirm.getByRole('button', { name: /^delete$/i }).click();
    await expect(page.getByText('The user has been deleted')).toBeVisible({ timeout: 15_000 });

    // RE-READ #1 — server side.
    const after = await apiMembers(page.request);
    expect(after.map((m) => m.email)).not.toContain(WRITE_PATH_EMAIL);
    expect(after).toHaveLength(before.length - 1);
    // The seeded personas must be untouched: DELETE removes project membership
    // for the ids it was given, and nothing else.
    for (const member of SEEDED_MEMBERS) {
      expect(after.map((m) => m.email)).toContain(member.email);
    }

    // RE-READ #2 — the grid after a full reload.
    await page.goto(BASE_URL + '/app/settings/users');
    await expect(page.getByRole('grid')).toBeVisible({ timeout: 15_000 });
    await expect(
      page.getByRole('grid').getByRole('row').filter({ hasText: WRITE_PATH_EMAIL }),
    ).toHaveCount(0);
  });

  /* ─── #188: the same PUT, driven as a BATCH ────────────────────────────────
   *
   * Nested inside `#130 write path` rather than sitting beside it, and that
   * nesting is load-bearing, not cosmetic: `fullyParallel` splits a spec file
   * across workers, and J22e/J22g assert `after).toHaveLength(before.length ±
   * 1)`. A sibling describe whose `beforeAll` invites two addresses would land
   * between those two reads often enough to make both tests flake for reasons
   * nothing in them mentions. Serial mode is inherited from the parent, so the
   * setup below cannot run until J22g has finished.
   */
  test.describe('batch edit', () => {
    /**
     * Two addresses of J22h's own. Reusing e2e-admin/e2e-member is not an
     * option: their roles are what every other spec's RBAC resolves through,
     * and the `settings-users` visual baseline is a screenshot of this grid.
     */
    const BATCH_EMAILS = [
      `${AUTOTEST_PREFIX}batch-a@autotest.local`,
      `${AUTOTEST_PREFIX}batch-b@autotest.local`,
    ] as const;

    /** What the pair is invited with, and what the batch edit must REPLACE. */
    const BATCH_ROLE_BEFORE = 'viewer';
    /**
     * What the batch edit sets. Deliberately NOT `editor`: e2e-member holds
     * `editor`, so a handler that applied the update to the whole project
     * instead of to the ids it was given would leave that persona looking
     * untouched. With `admin` as the target, such a bug moves e2e-member off
     * `editor` and the assertion below sees it.
     */
    const BATCH_ROLE_AFTER = 'admin';

    test.beforeAll(async ({ browser }) => {
      const context = await browser.newContext({ storageState: STORAGE_STATE.member });
      // An aborted run may have left either address behind; POST reports an
      // address that is already a member as an error and 400s the whole call.
      await removeMembersByEmail(context.request, BATCH_EMAILS);
      await inviteMembers(context.request, BATCH_EMAILS, [BATCH_ROLE_BEFORE]);
      await context.close();
    });

    test.afterAll(async ({ browser }) => {
      const context = await browser.newContext({ storageState: STORAGE_STATE.member });
      await removeMembersByEmail(context.request, BATCH_EMAILS);
      // Asserted, not hoped for — two leftover rows would change the
      // `settings-users` visual baseline and every row count in this file.
      const remaining = (await apiMembers(context.request)).map((row) => row.email);
      for (const email of BATCH_EMAILS) {
        expect(remaining).not.toContain(email);
      }
      await context.close();
    });

    test('J22h: a batch role edit writes to EVERY selected member and survives a full reload', async ({
      page,
    }) => {
      test.setTimeout(90_000);

      await page.goto(BASE_URL + '/app/settings/users');
      await expect(page.getByRole('grid')).toBeVisible({ timeout: 15_000 });

      // Precondition, off the server: both addresses are members and both
      // carry exactly the role the edit is about to replace. Without this the
      // post-state below could be satisfied by an edit that never happened.
      const before = await apiMembers(page.request);
      const targets = BATCH_EMAILS.map((email) => {
        const member = before.find((row) => row.email === email);
        expect(member, `${email} was not invited by the setup step`).toBeTruthy();
        expect(member?.roles).toEqual([BATCH_ROLE_BEFORE]);
        return member!;
      });

      // ── select two rows ────────────────────────────────────────────────
      // `useUsersActions` switches from `singleAction` to `batchAction` at
      // `selectedUsers.length >= 2`, so the second checkbox is what puts the
      // page into the mode this test exists for.
      for (const email of BATCH_EMAILS) {
        await selectRowByEmail(page, email);
      }

      /*
       * The HEADER copy of the control, located structurally.
       *
       * `page.getByRole('button', {name: 'Edit role'})` matches THREE elements
       * here, not one: `UsersTable`'s actions column renders a per-row pencil
       * for every row in `actions.edit.userIds`, and in batch mode that array
       * holds both selected ids. Those per-row buttons are not the batch
       * control — each opens a dialog seeded with its own row's roles — so
       * `.first()`/`.last()` would silently pick a different code path
       * depending on DOM order. The toolbar is the smallest element carrying
       * both the search box and the Invite button (`UsersPageHeader`), and the
       * count assertion is what turns a future layout change into a failure
       * here rather than into a test that quietly edits one user.
       */
      const toolbar = page
        .locator('div')
        .filter({ has: page.getByPlaceholder('Search users…') })
        .filter({ has: page.locator('button[title="Invite users"]') })
        .last();
      const headerEdit = toolbar.getByRole('button', { name: 'Edit role' });
      await expect(headerEdit).toHaveCount(1);
      await headerEdit.click();

      const editDialog = page.getByRole('dialog').filter({ hasText: 'Edit roles' });
      await expect(editDialog).toBeVisible();

      /*
       * The batch dialog starts from an EMPTY selection — old-app parity with
       * `EditUsersButton.jsx`'s `originalRoles={isBatchEdit ? [] : user?.roles
       * || []}`, which the port keeps by giving `batchAction.edit` no
       * `userRoles` at all (useUsersActions.ts) so `EditUsersButton` falls
       * through to `?? []`.
       *
       * This is also the sharpest single discriminator between batch and
       * single mode: both selected members hold `viewer`, so a dialog that had
       * been seeded from the first selected user would show `viewer` ticked.
       * Asserting the ticks rather than the collapsed input's text on purpose —
       * MUI's `Select` renders a zero-width space when `renderValue` returns
       * '', so `toHaveText('')` is not the check it looks like.
       */
      const save = editDialog.getByRole('button', { name: /^save$/i });
      await expect(save).toBeDisabled();

      await editDialog.getByRole('combobox').click();
      for (const role of SEEDED_ROLES) {
        await expect(
          page.getByRole('option', { name: role, exact: true }).getByRole('checkbox'),
        ).not.toBeChecked();
      }

      // ── make the change ────────────────────────────────────────────────
      await page.getByRole('option', { name: BATCH_ROLE_AFTER, exact: true }).click();
      await page.keyboard.press('Escape');
      await expect(save).toBeEnabled();

      /*
       * The request itself. Not a status assertion — a status is exactly what
       * the #130 no-op satisfied — but the one place where "the client still
       * speaks the batch form" is observable. `useBatchEditUsers` joins the
       * selected ids with ',' into a single `userId` string
       * (entities/user/model/useEditUser.ts), which is the shape
       * `UsersUpdate` splits server-side; a client that regressed to sending
       * one id would still get a 200 and still show the success toast, and
       * only the second member's roles would give it away.
       */
      const putRequest = page.waitForRequest(
        (req) => req.method() === 'PUT' && req.url().includes('/admin/users/default/'),
        { timeout: 15_000 },
      );
      await save.click();
      const sent = JSON.parse((await putRequest).postData() ?? '{}') as {
        userId?: string;
        roles?: string[];
      };
      expect(sent.userId, 'the batch PUT must carry a comma-joined id list').toContain(',');
      expect(sent.userId?.split(',').sort()).toEqual(targets.map((m) => m.id).sort());
      expect(sent.roles).toEqual([BATCH_ROLE_AFTER]);

      // Waited on only so the mutation has certainly settled; the toast is the
      // same one the no-op handler used to produce.
      await expect(page.getByText('The user has been edited successfully')).toBeVisible({
        timeout: 15_000,
      });

      // ── RE-READ #1: the server, with no client code in the path ────────
      const after = await apiMembers(page.request);
      for (const email of BATCH_EMAILS) {
        const member = after.find((row) => row.email === email);
        expect(member, `PUT returned success but ${email} is no longer a member`).toBeTruthy();
        // REPLACEMENT, and for BOTH: a handler that split the id list but
        // merged instead of replacing reports ['admin','viewer'] here, and one
        // that took only the first id leaves batch-b on ['viewer'].
        expect([...(member?.roles ?? [])].sort(), `${email} after the batch edit`).toEqual([
          BATCH_ROLE_AFTER,
        ]);
      }
      // Blast radius: the write touched the two ids it was given and nothing
      // else. e2e-member is still `editor`, which `admin` would have overwritten.
      for (const seeded of SEEDED_MEMBERS) {
        const member = after.find((row) => row.email === seeded.email);
        expect(member?.roles, `seeded ${seeded.email} must be untouched`).toEqual([seeded.role]);
      }
      expect(after).toHaveLength(before.length);

      // ── RE-READ #2: a full page load, so the rows can only come from the
      // server — `onEditSuccess` does not refetch, and react-query's cache is
      // gone across a `goto`. ─────────────────────────────────────────────
      await page.goto(BASE_URL + '/app/settings/users');
      const grid = page.getByRole('grid');
      await expect(grid).toBeVisible({ timeout: 15_000 });
      for (const email of BATCH_EMAILS) {
        const row = grid.getByRole('row').filter({ hasText: email });
        await expect(row).toHaveCount(1, { timeout: 15_000 });
        await expect(
          row.getByRole('gridcell', { name: BATCH_ROLE_AFTER, exact: true }),
        ).toBeVisible();
        await expect(
          row.getByRole('gridcell', { name: BATCH_ROLE_BEFORE, exact: true }),
        ).toHaveCount(0);
      }
    });
  });

  /**
   * J22i — the BULK invite: many addresses, one role, one submit, and an
   * answer per address.
   *
   * Serial with the rest of the write path, and with its own sweep, for the
   * same reason `batch edit` has one: the two addresses it adds would land in
   * every other row count in this file and in the `settings-users` visual
   * baseline.
   *
   * The batch is deliberately MIXED — one address nobody has, one that is
   * already a member — so the server answers 400 with the per-address array.
   * That is the shape the whole feature turns on: a 400 here does NOT mean the
   * request was rejected, because each address is written in its own
   * transaction. A build that rolled the batch back on the first failure, or a
   * client that treated the 400 as a total failure, both pass every
   * single-address case in this file and fail this one.
   */
  test.describe('bulk invite', () => {
    /** Two addresses of J22i's own. Never reuse the personas — see `batch edit`. */
    const BULK_NEW = `${AUTOTEST_PREFIX}bulk-new@autotest.local`;
    /** Already a member when the batch is submitted, so its row is `already_member`. */
    const BULK_EXISTING = `${AUTOTEST_PREFIX}bulk-existing@autotest.local`;
    /** Never a valid address, so its row is `invalid_email` and it is never written. */
    const BULK_MALFORMED = `${AUTOTEST_PREFIX}bulk-not-an-email`;

    test.beforeAll(async ({ browser }) => {
      const context = await browser.newContext({ storageState: STORAGE_STATE.member });
      await removeMembersByEmail(context.request, [BULK_NEW, BULK_EXISTING]);
      // BULK_EXISTING must already be a member for its row to be the
      // "already a member" case rather than a second invite.
      await inviteMembers(context.request, [BULK_EXISTING], ['viewer']);
      await context.close();
    });

    test.afterAll(async ({ browser }) => {
      const context = await browser.newContext({ storageState: STORAGE_STATE.member });
      await removeMembersByEmail(context.request, [BULK_NEW, BULK_EXISTING]);
      const remaining = (await apiMembers(context.request)).map((row) => row.email);
      for (const email of [BULK_NEW, BULK_EXISTING]) {
        expect(remaining).not.toContain(email);
      }
      await context.close();
    });

    test('J22i: a mixed batch invites the good addresses and names what happened to every one', async ({
      page,
    }) => {
      test.setTimeout(90_000);

      await page.goto(BASE_URL + '/app/settings/users');
      await expect(page.getByRole('grid')).toBeVisible({ timeout: 15_000 });

      // Precondition off the server, so the post-state cannot be satisfied by
      // a batch that never ran.
      const before = await apiMembers(page.request);
      expect(before.map((row) => row.email)).toContain(BULK_EXISTING);
      expect(before.map((row) => row.email)).not.toContain(BULK_NEW);

      await page.goto(BASE_URL + '/app/settings/users?inviteUsers=1');
      const dlg = inviteDialog(page);
      await expect(dlg).toBeVisible({ timeout: 15_000 });

      // ONE field, THREE addresses, mixed separators. A comma-only split would
      // read the newline-joined pair as a single malformed address.
      await dlg
        .getByRole('textbox', { name: /emails/i })
        .first()
        .fill(`${BULK_NEW}, ${BULK_EXISTING}\n${BULK_MALFORMED}`);

      // Every address is chipped, so an operator can see what was parsed.
      await expect(dlg.getByTestId(`invite-email-chip-${BULK_NEW}`)).toBeVisible();
      await expect(dlg.getByTestId(`invite-email-chip-${BULK_EXISTING}`)).toBeVisible();
      await expect(dlg.getByTestId(`invite-email-chip-${BULK_MALFORMED}`)).toBeVisible();

      // The malformed address blocks the submit, exactly as J22b's single one
      // does. Remove it and the batch becomes sendable — which is also the
      // proof that the chip and the disabled button agree about which address
      // is bad.
      await expect(dlg.getByRole('button', { name: /^invite$/i })).toBeDisabled();
      await dlg
        .getByRole('textbox', { name: /emails/i })
        .first()
        .fill(`${BULK_NEW}, ${BULK_EXISTING}`);

      await pickInviteRole(page, 'viewer');
      // The picked role is rendered back. It was not, before this journey:
      // `SingleSelect` was mounted with `value=""`.
      await expect(dlg.getByTestId('invite-role-chip-viewer')).toBeVisible();

      const inviteBtn = dlg.getByRole('button', { name: /^invite$/i });
      await expect(inviteBtn).toBeEnabled();
      await inviteBtn.click();

      // The per-address answer is RENDERED. Before the bulk port the dialog
      // closed here and the array went nowhere.
      const results = dlg.getByTestId('invite-results');
      await expect(results).toBeVisible({ timeout: 15_000 });
      await expect(results.getByTestId(`invite-result-${BULK_NEW}`)).toContainText('Invited');
      await expect(results.getByTestId(`invite-result-${BULK_EXISTING}`)).toContainText(
        'Already a member',
      );

      // RE-READ #1: straight off the server. The good address landed even
      // though the call answered 400.
      const after = await apiMembers(page.request);
      const written = after.find((row) => row.email === BULK_NEW);
      expect(
        written,
        `the batch answered per-address results but ${BULK_NEW} is not a member`,
      ).toBeTruthy();
      expect(written?.roles).toEqual(['viewer']);
      // The existing member keeps exactly the roles it had — an
      // `already_member` row must not re-grant anything.
      expect(after.find((row) => row.email === BULK_EXISTING)?.roles).toEqual(['viewer']);
      expect(after).toHaveLength(before.length + 1);
      expect(after.map((row) => row.email)).not.toContain(BULK_MALFORMED);

      // RE-READ #2: a full page load, so the row can only come from the server.
      await page.goto(BASE_URL + '/app/settings/users');
      const row = page.getByRole('grid').getByRole('row').filter({ hasText: BULK_NEW });
      await expect(row).toHaveCount(1, { timeout: 15_000 });
      await expect(row.getByRole('gridcell', { name: 'viewer', exact: true })).toBeVisible();
    });
  });
});
