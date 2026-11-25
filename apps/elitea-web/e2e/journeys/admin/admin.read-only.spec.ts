/**
 * Journey: the auditor walk (ADR-0059 decision 6, Verification; R3.4b,
 * issue 2613).
 *
 * The security auditor reads what it opens and changes nothing. Signed in as
 * the `security_auditor` persona (R3.13a), the walk follows the navigation
 * the capability document builds, so it has no page list of its own: a page
 * the document opens to the role is walked the day it appears. On every page
 * no write control is enabled; a page that renders a write control shows the
 * "View only" notice; and reading keeps working (the page renders, its
 * search, tabs and paging stay enabled).
 *
 * The review outcome form (R3.11, issue 2542) is the one write the auditor
 * holds; it is not merged, so the exception list is empty.
 *
 * Read-only: it changes nothing, so both engines run it in parallel.
 */
import { test, expect, type Locator, type Page } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.personas.security_auditor });

/**
 * A write control's accessible name. A filter's "Apply" reads, so only
 * "Apply to …" is a write. Matches `src/pages/admin/__tests__/viewOnly.ts`.
 *
 * Anchored to the start of the name, because a control names its action
 * first ("Delete role: x", "Bulk invite", "Re-validate"), while a row that
 * merely shows data can contain a verb anywhere. The audit trail lists
 * traces named after their routes ("DELETE /api/v2/admin/..."), so a name
 * that is an HTTP route is never a write control.
 */
const WRITE_CONTROL =
  /^(?!(?:get|put|post|patch|delete)\s+\/)\s*(?:bulk\s+|re-?|un)?(save|create|new|add|edit|delete|remove|clear|revoke|approve|reject|deny|invite|import|upload|reset|enable|disable|suspend|resume|rotate|grant|sync|adopt|publish|rename|bind|manage|test|validate|set|apply to)\b/i;

/** Write controls the auditor may use: the review outcome form, once R3.11 ships. */
const ALLOWED_WRITES: readonly RegExp[] = [];

/** A row checkbox of a plain table or an MUI X data grid; the grid's select-all box is excluded. */
const ROW_CHECKBOX = ['table', '[role="grid"]']
  .map((container) => `${container} input[type="checkbox"]:not([aria-label="Select all rows"])`)
  .join(', ');

/**
 * The write controls of the page: named buttons, and the row checkboxes of its
 * tables and data grids (a grid's select-all header box only selects).
 */
function writeControls(main: Locator): Locator {
  return main
    .getByRole('button', { name: WRITE_CONTROL })
    .or(main.locator(ROW_CHECKBOX));
}

async function enabledWrites(main: Locator): Promise<string[]> {
  const controls = writeControls(main);
  const found: string[] = [];
  for (const control of await controls.all()) {
    if (!(await control.isEnabled())) continue;
    const name = (await control.getAttribute('aria-label')) ?? (await control.textContent()) ?? '';
    if (!ALLOWED_WRITES.some((allowed) => allowed.test(name))) found.push(name.trim());
  }
  return found;
}

async function navItems(page: Page): Promise<{ id: string; href: string }[]> {
  const nav = page.getByTestId('admin-nav');
  await expect(nav).toBeVisible({ timeout: 30_000 });
  await expect(nav.getByRole('link').first()).toBeVisible({ timeout: 20_000 });
  return nav.locator('a[data-testid^="admin-nav-item-"]').evaluateAll((anchors) =>
    anchors.map((anchor) => ({
      id: (anchor.getAttribute('data-testid') ?? '').replace('admin-nav-item-', ''),
      href: anchor.getAttribute('href') ?? '',
    })),
  );
}

test('the security auditor walks every page it opens and finds no enabled write control', async ({ page }) => {
  await page.goto(`${BASE_URL}/admin/app/`, { waitUntil: 'domcontentloaded' });
  const items = await navItems(page);
  // The document opens Access (access.roles.view, ADR-0059 R3.6) and Audit
  // Trail at least, both at read.
  const ids = items.map((entry) => entry.id);
  expect(ids).toEqual(expect.arrayContaining(['access', 'audit-trail']));

  for (const entry of items) {
    await test.step(entry.id, async () => {
      await page.getByTestId(`admin-nav-item-${entry.id}`).click();
      const main = page.getByRole('main');
      await expect(main.getByRole('heading', { level: 1 })).toBeVisible({ timeout: 20_000 });
      await expect(page.getByTestId('admin-item-denied')).toHaveCount(0);
      // Settle: the page's first read is done once its progress bar is gone.
      await expect(main.getByRole('progressbar')).toHaveCount(0, { timeout: 20_000 });
      if ((await writeControls(main).count()) > 0) {
        await expect(page.getByTestId('admin-view-only')).toBeVisible({ timeout: 20_000 });
      }
      await expect.poll(() => enabledWrites(main), { timeout: 20_000 }).toEqual([]);
    });
  }
});

test('the security auditor reads Access: the role definitions, view only, and no way to the matrix', async ({ page }) => {
  await page.goto(`${BASE_URL}/admin/app/access`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('admin-access-page')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('admin-view-only')).toBeVisible({ timeout: 20_000 });
  // Every built-in role is listed, with what it opens; the auditor's own row
  // shows Access at read.
  await expect(page.getByTestId('admin-access-role-super_admin')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('admin-access-item-security_auditor-access')).toHaveAttribute('data-level', 'read');
  // The auditor gives no role to anyone, and the permission matrix is not its to read.
  await expect(page.getByTestId('admin-access-assignable-admin')).toHaveAttribute('data-assignable', 'false');
  await expect(page.getByTestId('admin-access-matrix-link')).toHaveCount(0);
  await expect.poll(() => enabledWrites(page.getByRole('main')), { timeout: 20_000 }).toEqual([]);
});

test('the security auditor opening Roles by its address finds it view only: notice, disabled writes, working tabs and search', async ({
  page,
}) => {
  // No link leads here (issue 3042): the page still opens by its address,
  // under Access at read, so its writes must be disabled and say why.
  await page.goto(`${BASE_URL}/admin/app/roles`, { waitUntil: 'domcontentloaded' });
  const notice = page.getByTestId('admin-view-only');
  await expect(notice).toBeVisible({ timeout: 30_000 });
  await expect(notice).toContainText('View only');
  await expect(page.getByTestId('admin-view-only-roles')).toContainText('Platform administrator', {
    timeout: 20_000,
  });

  const main = page.getByRole('main');
  await expect(page.getByTestId('admin-roles-new-role')).toBeDisabled();
  // The matrix is not the auditor's to read: GET /admin/permissions needs
  // configuration.roles.permissions.view, so the server refuses it (by
  // decision, issue 3042).
  await expect.poll(() => enabledWrites(main), { timeout: 20_000 }).toEqual([]);

  // Reading still works: the tabs switch, and the standard tab's Apply is disabled.
  await page.getByRole('tab', { name: 'Standard Roles' }).click();
  await expect(page.getByTestId('admin-roles-apply')).toBeDisabled({ timeout: 20_000 });
  await page.getByTestId('admin-roles-search').fill('roles');
  await expect.poll(() => enabledWrites(main), { timeout: 20_000 }).toEqual([]);
});
