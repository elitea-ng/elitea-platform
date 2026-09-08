/**
 * J40: a project that crosses its soft-alert threshold warns before the
 *      gateway starts refusing it (issue 312).
 *
 * ## What was missing
 *
 * The budget threshold endpoints have existed since #322 with NOTHING calling
 * them — `internal/api/v2/admin/config_schemas.go` says exactly that of the
 * whole surface. The hard refusal at 100% was ported and the warning before it
 * was not, so a project went from "fine" to "every call refused" with no signal
 * in between. The reference carries the warning as a notification type
 * (`BudgetThresholdReached`, routing to the project's Usage page); this
 * platform answers the crossing on the budget read itself (`warning_active`)
 * and the shell renders it as a banner.
 *
 * ## What this journey can and cannot arrange
 *
 * SPEND is not writable through any API. `gateway.llm_budget_accumulators` is
 * the LLM gateway's write-back target, and the budgets API authors only the
 * ceiling and the threshold. So the accrued cost is a seed fixture
 * (`scripts/e2e-stack.sh`, 9.00 USD for the current month on
 * `e2e-budget-<engine>`) and the journey sets the LIMIT through the admin PUT,
 * which is the half a product surface owns. A run that changed the fixture's
 * cost would be arranging the very thing it asserts.
 *
 * ONE PROJECT PER ENGINE. `fullyParallel` is on, and this journey PUTs and then
 * DELETEs a budget on its project — two engines doing that to one row would
 * each observe the other's window.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

/** The seeded budget project for this engine, carrying 9.00 USD of spend. */
function budgetProject(projectName: string): string {
  return projectName === 'chromium' ? 'e2e-budget-chromium' : 'e2e-budget-webkit';
}

/** The read this journey drives. Only the four fields the banner uses. */
interface BudgetState {
  readonly warning_pct: number;
  readonly percent_used: number | null;
  readonly warning_active: boolean;
  readonly enabled: boolean;
}

async function projectIdByName(page: Page, name: string): Promise<number> {
  const listing = await page.request.get(
    `${BASE_URL}/api/v2/admin/projects/administration?search=${name}&limit=20&offset=0`,
  );
  expect(listing.status(), await listing.text()).toBe(200);
  const rows = ((await listing.json()) as { rows: { id: number; name: string }[] }).rows;
  const project = rows.find((row) => row.name === name);
  expect(project, `the seed must carry the project ${name}`).toBeTruthy();
  return project?.id ?? 0;
}

/** `PUT /elitea_core/project_budget/administration/{id}/budget` — the admin write. */
async function setBudget(page: Page, projectId: number, limit: number, threshold: number) {
  const response = await page.request.put(
    `${BASE_URL}/api/v2/elitea_core/project_budget/administration/${projectId}/budget`,
    { data: { monthly_limit: limit, soft_alert_pct: threshold, enabled: true } },
  );
  expect(response.status(), await response.text()).toBe(200);
}

/** `GET /elitea_core/project_budget/prompt_lib/{id}/budget` — the read the banner polls. */
async function readBudget(page: Page, projectId: number): Promise<BudgetState> {
  const response = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/project_budget/prompt_lib/${projectId}/budget`,
  );
  expect(response.status(), await response.text()).toBe(200);
  return (await response.json()) as BudgetState;
}

/** Leave the stack as it was found, so the journey is re-runnable. */
async function clearBudget(page: Page, projectId: number) {
  const response = await page.request.delete(
    `${BASE_URL}/api/v2/elitea_core/project_budget/administration/${projectId}/budget`,
  );
  expect(response.status(), await response.text()).toBe(200);
}

/** Switch the shell to `name` through the product's own switcher. */
async function selectProject(page: Page, name: string): Promise<void> {
  const trigger = page.getByRole('button', { name: /Project:/ });
  await expect(trigger).toBeVisible({ timeout: 20_000 });
  await trigger.click();
  const listbox = page.getByRole('listbox');
  await expect(listbox).toBeVisible({ timeout: 10_000 });
  await listbox.getByRole('option', { name }).click();
  await expect(trigger).toHaveAccessibleName(new RegExp(`Project:\\s*${name}`), { timeout: 20_000 });
}

test('J40: crossing the soft-alert threshold raises the banner, and dismissing it holds', async ({ page }, testInfo) => {
  const projectName = budgetProject(testInfo.project.name);
  const projectId = await projectIdByName(page, projectName);

  // BELOW the threshold first, and asserted rather than assumed. A journey
  // that only ever saw the warning state could not tell a working banner from
  // one that is always on — which is the banner nobody reads.
  await setBudget(page, projectId, 1000, 80);
  const quiet = await readBudget(page, projectId);
  expect(quiet.warning_pct).toBe(80);
  expect(quiet.warning_active, `9.00 of 1000 is under 80% (${quiet.percent_used}%)`).toBe(false);

  await page.goto(BASE_URL + '/app/', { waitUntil: 'domcontentloaded' });
  await selectProject(page, projectName);
  await expect(page.getByTestId('budget-warning-banner')).toHaveCount(0);

  // Now lower the ceiling so the SAME spend crosses the SAME threshold. The
  // spend never moves, so the only variable is the limit an operator authored.
  await setBudget(page, projectId, 10, 80);
  const warned = await readBudget(page, projectId);
  expect(warned.warning_active, `9.00 of 10 is over 80% (${warned.percent_used}%)`).toBe(true);

  // A reload rather than a wait on the poll: the banner polls once a minute,
  // and a journey that waited that out would be a minute of nothing.
  await page.reload({ waitUntil: 'domcontentloaded' });
  const banner = page.getByTestId('budget-warning-banner');
  await expect(banner).toBeVisible({ timeout: 20_000 });
  await expect(banner).toContainText(projectName);
  await expect(banner).toContainText('% of its monthly budget');
  // The click-through the reference's notification carries: the project's own
  // Usage page.
  await expect(page.getByTestId('budget-warning-usage-link')).toHaveAttribute(
    'href',
    '/settings/usage',
  );

  // Dismissal is per session and per billing period, so it survives a reload
  // of the same tab. A banner that came back on every navigation would be
  // worse than none.
  await banner.getByRole('button', { name: 'Dismiss' }).click();
  await expect(banner).toHaveCount(0);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('button', { name: /Project:/ })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('budget-warning-banner')).toHaveCount(0);

  // The server still says the threshold is crossed — the dismissal silenced
  // the banner, not the budget.
  expect((await readBudget(page, projectId)).warning_active).toBe(true);

  await clearBudget(page, projectId);
  expect((await readBudget(page, projectId)).warning_active, 'no ceiling, no warning').toBe(false);
});
