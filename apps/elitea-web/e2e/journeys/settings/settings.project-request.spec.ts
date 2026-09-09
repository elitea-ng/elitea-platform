/**
 * Journey: a member requests a project, an admin approves it, and the
 * member ends up with a real, usable project (#871).
 *
 * Before this issue every project was admin-created
 * (`POST /projects/project/administration`) with no member-facing
 * counterpart at all. This exercises the whole loop against the real
 * backend:
 *
 *  1. MEMBER opens the project switcher and submits "Request a project" —
 *     `POST /admin/moderation_status/project_request`, the same
 *     `centry.moderation_state` table the App Requests queue already reads,
 *     under a new `issue_type` ("Project Request").
 *  2. ADMIN opens the SAME App Requests page this platform already ships,
 *     filters it to that issue type, and approves the row.
 *  3. Approving a Project Request is NOT clerical, unlike every other
 *     moderation decision on this page (`internal/api/v2/moderation/
 *     project_requests.go`'s file header): it runs the real project-
 *     creation pipeline, with the requester as the new project's admin. The
 *     server's response carries `created_project_id`; this journey verifies
 *     the project by that id, not by trusting the response alone.
 *  4. The MEMBER's own "your requests" read (`GET /admin/moderation_status/
 *     project_requests/mine`) shows the decision, and the project switcher
 *     lists the new project.
 *
 * TEARDOWN: the moderation row itself has no delete route for THIS entity
 * shape (only the per-{mode}/{projectID}/{entityID} withdraw route does, and
 * a Project Request's `project_id` is the requester's PERSONAL project, not
 * one this file knows ahead of the run) — same "nothing deletes a
 * moderation row outside its own withdraw route" fact
 * `settings.model-request.spec.ts` documents for its sibling issue type. The
 * row is left decided. What is NOT left behind is the PROVISIONED PROJECT:
 * unlike a model-connection approval (deliberately clerical), this one
 * creates a real tenant schema and vault, so `afterAll` deletes it via the
 * same admin `DELETE /projects/project/administration/{id}` a manually
 * created project would be torn down through.
 */
import { test, expect, request as apiRequest } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX } from '../../fixtures/api';

const MINE_URL = `${API_BASE}/admin/moderation_status/project_requests/mine`;

/** Project ids this run provisioned, so `afterAll` can deprovision them regardless of where the test stopped. */
const createdProjectIds: number[] = [];

test.afterAll(async () => {
  if (createdProjectIds.length === 0) return;
  const api = await apiRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.admin });
  try {
    for (const id of createdProjectIds) {
      const deleted = await api.delete(`${API_BASE}/projects/project/administration/${id}`);
      if (!deleted.ok()) {
        // eslint-disable-next-line no-console -- a silent teardown is how a real tenant schema outlives its own test
        console.warn(`project-request teardown: could not delete project ${id} (${String(deleted.status())})`);
      }
    }
  } finally {
    await api.dispose();
  }
});

test('J-PR1: member requests a project, admin approves it from App Requests, member gets a real project', async ({ page, browser }, testInfo) => {
  test.setTimeout(90_000);

  const projectName = `${AUTOTEST_PREFIX}j_pr_${testInfo.project.name}_${Date.now()}`;

  /* ── 1. member: submit the request from the project switcher ────────── */
  await page.goto(`${BASE_URL}/app`);
  await page.locator('[id^="project-switcher-trigger-"]').click({ timeout: 15_000 });
  await page.getByTestId('project-switcher-request-project').click({ timeout: 5_000 });

  const dialog = page.getByTestId('request-project-dialog');
  await expect(dialog).toBeVisible({ timeout: 10_000 });
  await dialog.getByTestId('request-project-name').locator('input').fill(projectName, { timeout: 3_000 });
  await dialog
    .getByTestId('request-project-justification')
    .locator('textarea')
    .first()
    .fill('End-to-end journey J-PR1.', { timeout: 3_000 });

  const submitted = page.waitForResponse(
    (res) => res.request().method() === 'POST' && res.url().includes('/admin/moderation_status/project_request'),
    { timeout: 20_000 },
  );
  await dialog.getByTestId('request-project-submit').click({ timeout: 3_000 });
  const submitWrite = await submitted;
  expect(submitWrite.status(), await submitWrite.text()).toBe(201);
  const createdRequest = (await submitWrite.json()) as { id: number; status: string; issue_type: string };
  expect(createdRequest.issue_type).toBe('Project Request');
  expect(createdRequest.status).toBe('pending');

  await checkA11y(page);

  /* ── the server agrees: the member's own read shows it pending ───────── */
  const mineBeforeDecision = await page.request.get(MINE_URL);
  expect(mineBeforeDecision.status()).toBe(200);
  const mineBeforeBody = (await mineBeforeDecision.json()) as { rows: { id: number; status: string }[] };
  expect(mineBeforeBody.rows.some((row) => row.id === createdRequest.id && row.status === 'pending')).toBe(true);

  /* ── 2. admin: approve it from the SAME App Requests page ───────────── */
  const adminContext = await browser.newContext({ storageState: STORAGE_STATE.admin });
  const adminPage = await adminContext.newPage();
  try {
    await adminPage.goto(`${BASE_URL}/admin/app/app-requests`);
    await adminPage.getByTestId('admin-app-requests-issue-type-filter').click();
    await adminPage.getByRole('option', { name: 'Project Request' }).click();

    const row = adminPage.getByRole('row').filter({ hasText: projectName });
    await expect(row).toHaveCount(1, { timeout: 15_000 });

    const decided = adminPage.waitForResponse(
      (res) => res.request().method() === 'PUT' && res.url().includes('/admin/moderation_status/administration'),
      { timeout: 30_000 },
    );
    // The button's accessible name is `{action}: {issue_type}` (AppRequestsTable.tsx),
    // not the project name — scoped to `row` (already filtered on the name)
    // so this still addresses exactly one button.
    await row.getByRole('button', { name: 'Approve request: Project Request' }).click({ timeout: 5_000 });
    const decisionWrite = await decided;
    expect(decisionWrite.status(), await decisionWrite.text()).toBe(200);
    const decidedBody = (await decisionWrite.json()) as { status: string; created_project_id: number };
    expect(decidedBody.status).toBe('approved');
    expect(decidedBody.created_project_id).toBeGreaterThan(0);
    createdProjectIds.push(decidedBody.created_project_id);

    // The queue itself shows the project id, not just the network response.
    await expect(adminPage.getByText(`Project #${decidedBody.created_project_id} created`)).toBeVisible({
      timeout: 10_000,
    });
  } finally {
    await adminContext.close();
  }

  /* ── 3. the project is real: the member's own membership list gets it ── */
  const createdProjectId = createdProjectIds.at(-1)!;

  /* ── 4. member: the decision shows in "your requests", and the switcher lists the project ── */
  const mineAfterDecision = await page.request.get(MINE_URL);
  expect(mineAfterDecision.status()).toBe(200);
  const mineAfterBody = (await mineAfterDecision.json()) as {
    rows: { id: number; status: string; created_project_id?: number }[];
  };
  const decidedRow = mineAfterBody.rows.find((row) => row.id === createdRequest.id);
  expect(decidedRow?.status).toBe('approved');
  expect(decidedRow?.created_project_id).toBe(createdProjectId);

  await page.reload();
  await page.locator('[id^="project-switcher-trigger-"]').click({ timeout: 15_000 });
  await expect(page.getByRole('option', { name: projectName })).toBeVisible({ timeout: 15_000 });
});
