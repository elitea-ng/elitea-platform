/**
 * Journey 33: The admin Schedules tab shows the platform cron table the
 *             scheduler actually runs, and its switch really flips a row
 *             (JRNY-033)
 *
 * ## Why this journey needs to exist at all
 *
 * Before unit A14 this page's PUT had no route, and its GET read something else
 * entirely: it queried the TENANT schema for application versions carrying
 * `meta->'trigger'`, hardcoded `enabled: true`, swallowed every error into an
 * empty list, and short-circuited on `projectID == "0"` — the only projectID the
 * admin page ever sends. A journey that asserted "a schedules table is present"
 * would have passed against all of that, because the table would have been
 * present and empty.
 *
 * So the assertions here are against things only a working server can produce:
 *
 *  - `e2e_schedule_probe_<engine>` is seeded into `centry.schedule` by
 *    `scripts/e2e-stack.sh seed` and exists in no bundle. Seeing it proves the
 *    listing reads that table — the one `services/elitea-scheduler` polls — and
 *    not trigger metadata.
 *  - It is seeded INACTIVE, and the page must render it inactive. The pre-A14
 *    handler could not represent that state at all.
 *  - The write is verified by a RELOAD and a re-read, never by the toast. A
 *    handler that answered 200 and wrote nothing is the exact defect #130/#180
 *    shipped twice.
 *  - `rpc_func` is asserted to be READ-ONLY. It names an internal platform RPC
 *    that runs unattended with full privilege and no caller identity, so the
 *    absence of an editor for it is a security property, not a layout detail.
 *
 * The journey restores what it changed, so it is re-runnable against a stack
 * that was not re-seeded.
 */
import { test as adminTest, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

// Serial, and not because these are slow. `fullyParallel` is on, and every test
// below reads or writes this engine's ONE seeded row — J33c enables it and
// disables it again, J33 and J33b assert it is disabled, J33d asserts its cron
// is untouched. Run concurrently they would pass or fail on scheduling luck.
// This orders them within a browser project; `seededSchedule` below is what
// keeps the two projects out of each other's way.
adminTest.describe.configure({ mode: 'serial' });

/**
 * Seeded by `scripts/e2e-stack.sh seed` — see its `centry.schedule` block.
 *
 * ONE ROW PER BROWSER PROJECT. `describe.configure({ mode: 'serial' })` above
 * orders these tests within a project; it does nothing across them, and
 * chromium and webkit run this file concurrently. Sharing a row would have one
 * engine's toggle land between the other's write and its re-read — a failure
 * that reproduces once in ten runs and blames the product.
 */
function seededSchedule(projectName: string): string {
  return `e2e_schedule_probe_${projectName}`;
}
const SEEDED_FUNCTION = 'e2e_schedule_probe_noop';
const SEEDED_CRON = '0 4 * * *';

async function openSchedules(page: Page, schedule: string): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/schedules', { waitUntil: 'domcontentloaded' });
  expect(response?.status(), 'the admin SPA must serve the schedules route, not 404').toBeLessThan(400);
  await expect(page.getByRole('button', { name: schedule })).toBeVisible({ timeout: 20_000 });
}

function probeSwitch(page: Page, schedule: string) {
  return page.getByRole('switch', { name: `Schedule enabled: ${schedule}` });
}

adminTest('J33: the listing is the platform cron table, read from the database', async ({ page }, testInfo) => {
  const SEEDED_SCHEDULE = seededSchedule(testInfo.project.name);
  await openSchedules(page, SEEDED_SCHEDULE);

  // From `centry.schedule`. The read is gated on
  // `configuration.scheduling.schedules.view`; the admin persona holds it via
  // the seed, so a 403 here would mean the gate and the seed have drifted apart.
  await expect(page.getByText('Failed to load schedules.')).toHaveCount(0);

  const row = page.getByRole('row').filter({ hasText: SEEDED_SCHEDULE });
  // The FUNCTION is on screen: it is the only thing that says what the schedule
  // does, and the handler this replaced did not emit it at all.
  await expect(row.getByText(SEEDED_FUNCTION)).toBeVisible();
  await expect(row.getByText(SEEDED_CRON)).toBeVisible();
  // Seeded inactive, and rendered inactive. The pre-A14 read pinned this to
  // `true`, so a schedule that is off could not be shown as off.
  await expect(probeSwitch(page, SEEDED_SCHEDULE)).not.toBeChecked();
  // Never run, and said so rather than shown as a date.
  await expect(row.getByText('Never')).toBeVisible();

  await checkA11y(page);
});

adminTest('J33b: what a schedule INVOKES cannot be edited from this page', async ({ page }, testInfo) => {
  const SEEDED_SCHEDULE = seededSchedule(testInfo.project.name);
  await openSchedules(page, SEEDED_SCHEDULE);
  const row = page.getByRole('row').filter({ hasText: SEEDED_SCHEDULE });

  // The cron IS editable — it is a button that opens an inline editor.
  await expect(row.getByRole('button', { name: SEEDED_CRON })).toBeEnabled();

  // `rpc_func` is NOT. A scheduled run has no interactive principal: the
  // scheduler dispatches this function name unattended, with full platform
  // privilege and no caller identity. There is deliberately no control here,
  // and the server refuses the field even if one is forged.
  await expect(row.getByRole('button', { name: SEEDED_FUNCTION })).toHaveCount(0);
  await expect(row.getByRole('textbox', { name: SEEDED_FUNCTION })).toHaveCount(0);

  // The id is read back off the product's own listing, so the forged request
  // targets the REAL row rather than a guess the server would 404 anyway.
  const refused = await page.evaluate(async (id) => {
    const response = await fetch('/api/v2/scheduling/schedules/administration/0', {
      method: 'PUT',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, rpc_func: 'e2e_schedule_probe_hijacked' }),
    });
    return { status: response.status, body: await response.text() };
  }, await scheduleId(page, SEEDED_SCHEDULE));

  expect(
    refused.status,
    'retargeting what a schedule runs must be refused by the SERVER, not merely absent from the UI',
  ).toBe(400);
  expect(refused.body).toContain('rpc_func');

  // And the function is unchanged after the attempt.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('button', { name: SEEDED_SCHEDULE })).toBeVisible({ timeout: 20_000 });
  await expect(
    page.getByRole('row').filter({ hasText: SEEDED_SCHEDULE }).getByText(SEEDED_FUNCTION),
  ).toBeVisible();
});

adminTest('J33c: enabling a schedule reaches the server and survives a full reload', async ({ page }, testInfo) => {
  const SEEDED_SCHEDULE = seededSchedule(testInfo.project.name);
  await openSchedules(page, SEEDED_SCHEDULE);

  // The seed leaves it DISABLED; a test that started from "already enabled"
  // would prove nothing about the write.
  await expect(probeSwitch(page, SEEDED_SCHEDULE)).not.toBeChecked();
  await expect(
    probeSwitch(page, SEEDED_SCHEDULE),
    'the admin persona holds configuration.scheduling.schedules.edit, so the switch must be live',
  ).toBeEnabled();

  // The response is what proves the request was AUTHORISED, not merely sent.
  const [enableResponse] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/scheduling/schedules/administration/0') && r.request().method() === 'PUT',
    ),
    probeSwitch(page, SEEDED_SCHEDULE).click(),
  ]);
  expect(enableResponse.status(), 'the schedule write must be authorised server-side').toBe(200);

  // A FULL RELOAD, not a cache read. This is the assertion a handler that
  // answers 200 and writes nothing cannot pass.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('button', { name: SEEDED_SCHEDULE })).toBeVisible({ timeout: 20_000 });
  await expect(probeSwitch(page, SEEDED_SCHEDULE)).toBeChecked();

  // Put it back, and verify the revert the same way — a write that only ever
  // goes one way is not a working switch.
  const [disableResponse] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/scheduling/schedules/administration/0') && r.request().method() === 'PUT',
    ),
    probeSwitch(page, SEEDED_SCHEDULE).click(),
  ]);
  expect(disableResponse.status()).toBe(200);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('button', { name: SEEDED_SCHEDULE })).toBeVisible({ timeout: 20_000 });
  await expect(probeSwitch(page, SEEDED_SCHEDULE)).not.toBeChecked();
});

adminTest('J33d: the server refuses a cron its own scheduler could not run', async ({ page }, testInfo) => {
  const SEEDED_SCHEDULE = seededSchedule(testInfo.project.name);
  await openSchedules(page, SEEDED_SCHEDULE);
  const row = page.getByRole('row').filter({ hasText: SEEDED_SCHEDULE });

  await row.getByRole('button', { name: SEEDED_CRON }).click();
  const editor = page.getByRole('textbox', { name: 'Cron (5 fields)' });
  await expect(editor).toBeVisible();
  await editor.fill('@daily');

  // `@daily` parses only WITH robfig's `Descriptor` option, and the scheduler
  // builds its parser without it. Accepting it would persist a schedule that
  // silently never fires again — which on a platform job is indistinguishable
  // from a job with nothing to do.
  const [rejected] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().includes('/scheduling/schedules/administration/0') && r.request().method() === 'PUT',
    ),
    editor.press('Tab'),
  ]);
  expect(rejected.status(), 'an unrunnable cron must be refused, not stored').toBe(400);

  // The operator is told WHICH input was wrong, not "failed to save".
  await expect(page.getByTestId('admin-schedules-error')).toContainText('cron expression is invalid');

  // And the stored cron is untouched.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('button', { name: SEEDED_SCHEDULE })).toBeVisible({ timeout: 20_000 });
  await expect(
    page.getByRole('row').filter({ hasText: SEEDED_SCHEDULE }).getByText(SEEDED_CRON),
  ).toBeVisible();
});

/** Reads the seeded schedule's real id out of the listing the page just fetched. */
async function scheduleId(page: Page, schedule: string): Promise<number> {
  const id = await page.evaluate(async (name) => {
    const response = await fetch('/api/v2/scheduling/schedules/administration/0', {
      credentials: 'include',
    });
    const body = (await response.json()) as { rows?: { id: number; name: string }[] };
    return body.rows?.find((row) => row.name === name)?.id ?? null;
  }, schedule);
  expect(id, 'the seeded schedule must be present in the listing').not.toBeNull();
  return id as number;
}
