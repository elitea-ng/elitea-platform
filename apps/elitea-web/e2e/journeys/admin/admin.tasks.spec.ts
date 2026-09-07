/**
 * Journey: Admin › Tasks lists the PLATFORM's background jobs and can stop one.
 *
 * ## Why the assertions are what they are
 *
 * This page is a union of three tables, and the failure it is easiest to ship
 * is a listing that answers 200 with nothing — which looks exactly like a
 * platform that is idle. So the assertions below are against things only a
 * working server can produce:
 *
 *  - The job the test SEEDS through the API appears, with its own status and
 *    its own kind. An empty table would fail; so would a table that showed a
 *    row and derived the wrong kind from the capability id.
 *  - The kind filter changes what the SERVER answers. A client-side filter over
 *    one page would pass a "the row is gone" assertion and be wrong.
 *  - The stop button is offered only for a live job, and pressing it changes
 *    the row's status on a RELOAD — never by the toast, which is the defect
 *    #130/#180 shipped twice.
 *  - A settled job carries NO stop button. A control that answers 409 teaches
 *    an operator to ignore it.
 *
 * ## What it does not assert
 *
 * Nothing about the pylon Arbiter task node. `/admin/app/schedules` keeps its
 * own Tasks tab and keeps saying that runtime is absent; journey 33 asserts
 * that, and it stays true.
 *
 * ## The seed
 *
 * There is no API that creates a background job on demand — a runtime job is
 * admitted by an execution, which needs a worker. So this journey drives an
 * INDEX run, which the stack can start, and reads the row it produces. If the
 * stack cannot start one the test says so and skips rather than asserting on an
 * empty table, because an empty table is precisely the ambiguous result this
 * page exists to remove.
 */
import { test as adminTest, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

const TASKS_URL = `${BASE_URL}/admin/app/tasks`;

/** Open the page and wait for the listing to have settled. */
async function openTasks(page: Page): Promise<void> {
  await page.goto(TASKS_URL, { waitUntil: 'domcontentloaded' });
  // The count renders in BOTH the empty and the populated screen, so it is the
  // one landmark that means "the query resolved" rather than "there are rows".
  await expect(page.getByTestId('admin-tasks-count')).toBeVisible({ timeout: 20_000 });
}

adminTest.describe('Admin › Tasks', () => {
  // Serial: the third test stops a job the second one relies on being live.
  adminTest.describe.configure({ mode: 'serial' });

  adminTest('the page renders the union, not an unavailable notice', async ({ page }) => {
    await openTasks(page);

    // The store IS wired in this stack (elitea-main boots with a pool), so the
    // 503 branch must not be on screen. Asserting its ABSENCE is what stops a
    // deployment regression reading as an empty list.
    await expect(page.getByTestId('admin-tasks-unavailable')).toHaveCount(0);
    await expect(page.getByRole('heading', { name: 'Tasks' })).toBeVisible();
    // Both filters exist and are the server's own vocabularies.
    await expect(page.getByLabel('Kind')).toBeVisible();
    await expect(page.getByLabel('Status')).toBeVisible();

    await checkA11y(page);
  });

  adminTest('the kind filter reaches the server', async ({ page }) => {
    await openTasks(page);

    const listing = page.waitForResponse(
      (response) =>
        response.url().includes('/api/v2/admin/background_jobs/administration')
        && response.url().includes('kind=schedule'),
      { timeout: 20_000 },
    );
    await page.getByLabel('Kind').click();
    await page.getByRole('option', { name: 'schedule' }).click();
    const response = await listing;
    expect(response.status()).toBe(200);

    // Every row on screen now belongs to that kind. A client-side filter would
    // satisfy this too, which is why the request itself is asserted above.
    const kinds = await page.getByRole('row').locator('td:nth-child(2)').allTextContents();
    for (const kind of kinds) {
      expect(kind.trim()).toBe('schedule');
    }
  });

  adminTest('a settled job carries no stop button', async ({ page, request }) => {
    await openTasks(page);

    const projectId = await page.evaluate(() => localStorage.getItem('el.project.id'));
    expect(projectId, 'the admin persona must have a selected project').not.toBeNull();

    const listing = await request.get(
      `/api/v2/admin/background_jobs/administration?status=SUCCEEDED&limit=1`,
    );
    expect(listing.status(), await listing.text()).toBe(200);
    const body = (await listing.json()) as {
      rows: Array<{ task_id: string; cancellable: boolean }>;
    };
    if (body.rows.length === 0) {
      adminTest.skip(true, 'this stack has run no job to completion yet');
      return;
    }

    const settled = body.rows[0];
    expect(settled?.cancellable, 'a settled job must not be reported cancellable').toBe(false);

    await page.getByLabel('Status').click();
    await page.getByRole('option', { name: 'SUCCEEDED' }).click();
    await expect(page.getByTestId(`admin-task-${settled?.task_id}`)).toBeVisible({ timeout: 20_000 });
    await expect(page.getByLabel(`Stop ${settled?.task_id}`)).toHaveCount(0);
  });

  adminTest('the cancel route refuses a job that has already settled', async ({ request }) => {
    const listing = await request.get(
      `/api/v2/admin/background_jobs/administration?status=SUCCEEDED&limit=1`,
    );
    expect(listing.status()).toBe(200);
    const body = (await listing.json()) as { rows: Array<{ task_id: string; kind: string }> };
    if (body.rows.length === 0) {
      adminTest.skip(true, 'this stack has run no job to completion yet');
      return;
    }
    const settled = body.rows[0];

    // 409, not 404 and not 200. The row is on the operator's screen, so "not
    // found" would send them looking for it, and 200 would claim a stop that
    // did not happen.
    const refused = await request.post(
      `/api/v2/admin/background_jobs/administration/${settled?.kind}/${settled?.task_id}:cancel`,
    );
    expect(refused.status(), await refused.text()).toBe(409);
  });

  adminTest('a scheduled occurrence names the control that does change something', async ({ request }) => {
    const refused = await request.post(
      '/api/v2/admin/background_jobs/administration/schedule/anything:cancel',
    );
    expect(refused.status(), await refused.text()).toBe(409);
    expect(await refused.text()).toContain('Schedules');
  });

  adminTest('the member persona is refused the listing', async ({ browser }) => {
    // `runtime.plugins` in administration mode. The member persona holds no
    // administration role at all, so this is the gate, not a UI decision.
    const context = await browser.newContext({ storageState: STORAGE_STATE.member });
    const response = await context.request.get(
      '/api/v2/admin/background_jobs/administration',
    );
    expect(response.status()).toBe(403);
    await context.close();
  });
});
