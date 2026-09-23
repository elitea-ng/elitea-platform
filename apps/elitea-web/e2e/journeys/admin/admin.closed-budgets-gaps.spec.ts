/**
 * elitea_issues package C-admin — the Budgets-page enhancement epic (#6010's
 * family) confirmed absent by reading the current implementation, not just by
 * this journey.
 *
 * `apps/elitea-web/src/pages/admin/Budgets.tsx` + `AdminBudgetsTable.tsx` is a
 * real, recently-built page (gap G4) — per-project ceilings, per-member
 * budgets, an enforcement warning, tabs and a members drawer all exist and
 * are unit-tested (`Budgets.test.tsx`). What none of that code has is any of
 * the surface this cluster of CLOSED issues asks for:
 *
 *  - no Excel/export affordance anywhere on the page (`grep -ri
 *    'export|xlsx|excel'` over `Budgets.tsx`/`AdminBudgetsTable.tsx` — zero
 *    hits) — #6009, #6022, and #6178 (which presupposes an export that is
 *    "limited to 1000 rows"; there is no export to be limited, so #6178 is
 *    NA rather than DEFECT-CHECK against this app);
 *  - no bulk multi-project selection/action of any kind — #6008's bulk-edit
 *    half (its "remove per-user budgets from Personal Projects" half IS
 *    fixed — see `admin.budgets` unit tests "personal projects have no
 *    member-budget action");
 *  - tabs are still `All` / `Team (n)` / `Personal (n)` — the `All` tab is
 *    not removed and neither is renamed to `Team Projects` / `Personal
 *    Projects` — and there is no `ID` column and the spend column still
 *    reads `Spend this period`, not `Spent` — #6001, half of #6013;
 *  - `ErrorTrace` (`features/chat-messages/ui/error-trace/ErrorTrace.tsx`)
 *    has zero budget-aware branches — no friendly "This project's budget has
 *    been reached…" copy, no scope-specific usage link — so #6010, #6024,
 *    #6025 and #6026 (which all depend on #6024's backend error scope) are
 *    all still gaps;
 *  - no configurable warning-threshold fields, no budget notifications, and
 *    no warning banner above the chat input — #6011, #6012, #6014.
 *
 * One journey per group stands for the whole group, the same way
 * `agents.closed-entity-folders-gap.spec.ts` stands for its cluster.
 */
import { expect, test as adminTest } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

const BUDGETS_URL = `${BASE_URL}/admin/app/budgets`;

/* elitea_issues: #6009, #6022, #6178 — product gap: no Excel/export affordance exists anywhere on Admin > Budgets (#6178's "limited to 1000 rows" presupposes an export that does not exist at all) */
adminTest('J-budgets-gap: Budgets has no export-to-Excel affordance', async ({ page }) => {
  adminTest.fail(
    true,
    '#6009: product gap — Admin > Budgets has no Export-to-Excel action (nor does #6022\'s per-project Usage export exist); #6178 is NA against this app for the same reason',
  );

  await page.goto(BUDGETS_URL, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('heading', { name: 'Budgets' })).toBeVisible({ timeout: 20_000 });

  // The assertion this WOULD make once the feature exists.
  await expect(page.getByRole('button', { name: /export to excel/i })).toBeVisible({ timeout: 2_000 });
});

/* elitea_issues: #6001 — product gap: Budgets tabs/columns were never reworked to the spec (All tab kept, no Team/Personal Projects rename, no ID column, Spend not renamed to Spent) */
adminTest('J-budgets-gap: Budgets tabs and columns do not match the requested rework', async ({ page }) => {
  adminTest.fail(
    true,
    "#6001: product gap — the 'All' tab is still present (not removed), tabs are not renamed to 'Team Projects'/'Personal Projects', there is no ID column, and the spend column still reads 'Spend this period' rather than 'Spent'",
  );

  await page.goto(BUDGETS_URL, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('heading', { name: 'Budgets' })).toBeVisible({ timeout: 20_000 });

  await expect(page.getByRole('tab', { name: /^all$/i })).toHaveCount(0);
  await expect(page.getByRole('tab', { name: /team projects/i })).toBeVisible({ timeout: 2_000 });
  await expect(page.getByRole('columnheader', { name: /^id$/i })).toBeVisible({ timeout: 2_000 });
});

/* elitea_issues: #6010, #6024, #6025, #6026 — product gap: no surface renders a friendly budget-exceeded message; ErrorTrace has zero budget-aware branches */
adminTest('J-budgets-gap: chat ErrorTrace has no budget-exceeded friendly message', async ({ page }) => {
  adminTest.fail(
    true,
    '#6024: product gap — ErrorTrace (features/chat-messages/ui/error-trace) has no budget_exceeded scope handling at all, so #6010 (chat/agent/pipeline/skill/toolkit friendly copy), #6025 (Skill test panel) and #6026 (Pipeline run-state view) — all of which depend on #6024\'s backend error scope — remain unimplemented',
  );

  // The assertion this WOULD make once the feature exists: a data-testid the
  // component would render for a budget-scoped error. It does not exist
  // today at any scope, which is the point.
  await page.goto(`${BASE_URL}/app/chat`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('error-trace-budget-exceeded')).toBeVisible({ timeout: 2_000 });
});

/* elitea_issues: #6011, #6012, #6014 — product gap: no configurable warning thresholds, no budget notifications, no warning banner above chat input */
adminTest('J-budgets-gap: no configurable budget warning threshold field exists', async ({ page }) => {
  adminTest.fail(
    true,
    '#6012: product gap — no per-scope (Team/Personal/Member) configurable budget warning threshold exists anywhere in Admin > Features or Configuration; #6011 (threshold/limit notifications) and #6014 (warning banner above the chat input) are unimplemented for the same reason — there is no threshold config to drive them',
  );

  await page.goto(`${BASE_URL}/admin/app/features`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('heading', { name: 'Features' })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByLabel(/budget warning threshold/i)).toBeVisible({ timeout: 2_000 });
});
