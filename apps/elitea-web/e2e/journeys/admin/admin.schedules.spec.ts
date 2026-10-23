/**
 * Journey 33: The admin Schedules tab shows the routines the platform really
 *             runs, and its controls change them (JRNY-033, issue #1369)
 *
 * ## Why this journey needs to exist at all
 *
 * The tab used to list `centry.schedule`, a legacy table nothing executes: a
 * journey asserting "a schedules table is present" passed against a screen of
 * dead rows. The assertions here are against things only a working registry
 * can produce:
 *
 *  - `e2e.schedule.probe.<engine>` is seeded into
 *    `elitea_runtime.platform_routines` by `scripts/e2e-stack.sh seed`. Seeing
 *    it proves the listing reads the registry, not a constant.
 *  - elitea-main registers its own jobs when it starts, so
 *    `index.schedule.scan.v1` must be listed too.
 *  - Each write is verified by a RELOAD and a re-read, never by the toast. A
 *    handler that answered 200 and wrote nothing is the exact defect #130/#180
 *    shipped twice.
 *  - A schedule faster than the routine allows is refused by the SERVER.
 *
 * The journey restores what it changed, so it is re-runnable against a stack
 * that was not re-seeded.
 */
import { test as adminTest, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

// Serial: every test reads or writes this engine's ONE seeded row. This orders
// them within a browser project; the per-project row keeps the projects apart.
adminTest.describe.configure({ mode: 'serial' });

function probeRoutine(projectName: string): string {
  return `e2e.schedule.probe.${projectName}`;
}
const SEEDED_SCHEDULE = '24h';
const ROUTINES_PATH = '/admin/platform_routines/administration';

async function openSchedules(page: Page, routine: string): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/schedules', { waitUntil: 'domcontentloaded' });
  expect(response?.status(), 'the admin SPA must serve the schedules route, not 404').toBeLessThan(400);
  await expect(page.getByText(routine, { exact: true })).toBeVisible({ timeout: 20_000 });
}

function probeSwitch(page: Page, routine: string) {
  return page.getByRole('switch', { name: `Schedule enabled: ${routine}` });
}

function isRoutineWrite(r: { url(): string; request(): { method(): string } }): boolean {
  return r.url().includes(ROUTINES_PATH) && r.request().method() === 'PUT';
}

adminTest('J33: the listing is the routine registry, read from the database', async ({ page }, testInfo) => {
  const probe = probeRoutine(testInfo.project.name);
  await openSchedules(page, probe);
  await expect(page.getByText('Failed to load schedules.')).toHaveCount(0);

  // elitea-main registers its own jobs on start.
  await expect(page.getByText('index.schedule.scan.v1', { exact: true })).toBeVisible();

  const row = page.getByRole('row').filter({ hasText: probe });
  await expect(row.getByText(SEEDED_SCHEDULE, { exact: true })).toBeVisible();
  await expect(row.getByText('Default', { exact: true })).toBeVisible();
  await expect(probeSwitch(page, probe)).toBeChecked();

  await checkA11y(page);
});

adminTest('J33b: switching a routine off reaches the server and survives a reload', async ({ page }, testInfo) => {
  const probe = probeRoutine(testInfo.project.name);
  await openSchedules(page, probe);
  await expect(
    probeSwitch(page, probe),
    'the admin persona holds configuration.scheduling.schedules.edit, so the switch must be live',
  ).toBeEnabled();

  const [off] = await Promise.all([
    page.waitForResponse(isRoutineWrite),
    probeSwitch(page, probe).click(),
  ]);
  expect(off.status(), 'the routine write must be authorised server-side').toBe(200);

  // A FULL RELOAD, not a cache read.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByText(probe, { exact: true })).toBeVisible({ timeout: 20_000 });
  await expect(probeSwitch(page, probe)).not.toBeChecked();

  // Put it back and verify the revert the same way.
  const [on] = await Promise.all([
    page.waitForResponse(isRoutineWrite),
    probeSwitch(page, probe).click(),
  ]);
  expect(on.status()).toBe(200);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByText(probe, { exact: true })).toBeVisible({ timeout: 20_000 });
  await expect(probeSwitch(page, probe)).toBeChecked();
});

adminTest('J33c: a new schedule is stored, shown as custom, and can be reset', async ({ page }, testInfo) => {
  const probe = probeRoutine(testInfo.project.name);
  await openSchedules(page, probe);
  const row = page.getByRole('row').filter({ hasText: probe });

  await row.getByRole('button', { name: SEEDED_SCHEDULE }).click();
  const editor = page.getByRole('textbox', { name: 'Schedule (interval or cron)' });
  await editor.fill('12h');
  const [saved] = await Promise.all([page.waitForResponse(isRoutineWrite), editor.press('Tab')]);
  expect(saved.status()).toBe(200);

  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByText(probe, { exact: true })).toBeVisible({ timeout: 20_000 });
  const reloaded = page.getByRole('row').filter({ hasText: probe });
  await expect(reloaded.getByText('12h', { exact: true })).toBeVisible();
  await expect(reloaded.getByText('Custom (default 24h)')).toBeVisible();

  // An empty schedule restores the default.
  await reloaded.getByRole('button', { name: '12h' }).click();
  const reset = page.getByRole('textbox', { name: 'Schedule (interval or cron)' });
  await reset.fill('');
  const [restored] = await Promise.all([page.waitForResponse(isRoutineWrite), reset.press('Tab')]);
  expect(restored.status()).toBe(200);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(
    page.getByRole('row').filter({ hasText: probe }).getByText('Default', { exact: true }),
  ).toBeVisible({ timeout: 20_000 });
});

adminTest('J33d: the server refuses a schedule faster than the routine allows', async ({ page }, testInfo) => {
  const probe = probeRoutine(testInfo.project.name);
  await openSchedules(page, probe);
  const row = page.getByRole('row').filter({ hasText: probe });

  await row.getByRole('button', { name: SEEDED_SCHEDULE }).click();
  const editor = page.getByRole('textbox', { name: 'Schedule (interval or cron)' });
  // The probe allows no more often than once a minute.
  await editor.fill('10s');
  const [rejected] = await Promise.all([page.waitForResponse(isRoutineWrite), editor.press('Tab')]);
  expect(rejected.status(), 'a too-fast schedule must be refused, not stored').toBe(400);

  // The operator is told WHICH limit was hit, not "failed to save".
  await expect(page.getByTestId('admin-schedules-error')).toContainText('more often');

  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(
    page.getByRole('row').filter({ hasText: probe }).getByText(SEEDED_SCHEDULE, { exact: true }),
  ).toBeVisible({ timeout: 20_000 });
});

adminTest('J33e: an unknown routine id is a 404, not a silent success', async ({ page }, testInfo) => {
  await openSchedules(page, probeRoutine(testInfo.project.name));
  const result = await page.evaluate(async (path) => {
    const response = await fetch(`/api/v2${path}/e2e.no.such.routine`, {
      method: 'PUT',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled: false }),
    });
    return response.status;
  }, ROUTINES_PATH);
  expect(result).toBe(404);
});
