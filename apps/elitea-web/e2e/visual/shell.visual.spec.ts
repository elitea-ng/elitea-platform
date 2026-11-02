/**
 * `@visual` shots of the unified shell (PR 1553): the chrome every `/app/**`
 * page now shares, photographed on its own rather than only as the frame of
 * the route shots in `routes.visual.spec.ts`.
 *
 * - shell-list-toolbar-1280: the web top bar (breadcrumb, side-panel toggle)
 *   and the `PageToolbar` (tabs, search, Filters, "+ New agent") on a list page
 *   at 1280x800, where the side panel starts closed (it opens by default only
 *   from 1440px, so every 1602px route shot has it open).
 * - shell-list-filters-open-1280: the same page with the toolbar's Filters
 *   button pressed: the side panel open on its Filters tab.
 * - shell-list-rail-collapsed: the collapsed rail beside a list page.
 * - shell-account-menu-member / shell-account-menu-admin: the account menu
 *   open at the rail's foot; the administrator's carries "Admin portal".
 *
 * The chat page with the sidebar chat list is `chat-empty-state` in
 * `routes.visual.spec.ts`; the admin console's account menu and its nav at a
 * short window are in `admin.visual.spec.ts`; the pipeline editor at 1280 is in
 * `pipeline-editor.visual.spec.ts`.
 *
 * The same discipline as the route shots: shell landmark, a data landmark,
 * `settle()`, and the shared volatile masks (the chat list and the version
 * line among them).
 */
import { test, expect, type Page } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../playwright.config';
import { SNAPSHOT_TOLERANCE, chatLandmark, selectProject, settle, shellSettled, volatileRegions } from './lib/settle';

const AGENTS_LIST = '/app/agents/latest';

/** The agents list, resolved: its empty state renders only from an answered listing. */
async function openAgentsList(page: Page): Promise<void> {
  const projectName = await selectProject(page, undefined);
  await page.goto(BASE_URL + AGENTS_LIST, { waitUntil: 'domcontentloaded' });
  await shellSettled(page, projectName);
  await expect(page.getByText('No agents yet').first()).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('shell-top-bar')).toBeVisible();
  await expect(page.getByTestId('page-toolbar')).toBeVisible();
}

async function shoot(page: Page, name: string): Promise<void> {
  await settle(page);
  await expect(page).toHaveScreenshot(`${name}.png`, {
    fullPage: false,
    mask: volatileRegions(page),
    ...SNAPSHOT_TOLERANCE,
  });
}

test.describe('list page at 1280', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  test('@visual shell-list-toolbar-1280', async ({ page }) => {
    await openAgentsList(page);
    // Below 1440px the panel waits to be asked for.
    await expect(page.getByTestId('web-side-panel')).toHaveCount(0);
    await expect(page.getByTestId('shell-title')).toHaveText('Agents');
    await shoot(page, 'shell-list-toolbar-1280');
  });

  test('@visual shell-list-filters-open-1280', async ({ page }) => {
    await openAgentsList(page);
    await page.getByTestId('page-toolbar-filters').click();
    await expect(page.getByTestId('side-panel-body-filters')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId('side-panel-chip-filters')).toHaveAttribute('aria-selected', 'true');
    await shoot(page, 'shell-list-filters-open-1280');
  });
});

test('@visual shell-list-rail-collapsed', async ({ page }) => {
  await openAgentsList(page);
  await page.getByTestId('sidebar-toggle').click();
  // The project-name column renders only on the expanded rail.
  await expect(page.locator('button').filter({ hasText: 'Default Project' }).first()).toBeHidden({ timeout: 10_000 });
  await shoot(page, 'shell-list-rail-collapsed');
});

/** /app/chat, resolved, with the account menu open at the rail's foot. */
async function openAccountMenuOnChat(page: Page): Promise<void> {
  const projectName = await selectProject(page, undefined);
  await page.goto(BASE_URL + '/app/chat', { waitUntil: 'domcontentloaded' });
  await shellSettled(page, projectName);
  await expect(chatLandmark(page)).toBeVisible({ timeout: 20_000 });
  await page.getByTestId('shell-account').click();
  const menu = page.getByRole('menu', { name: 'Account menu' });
  await expect(menu).toBeVisible({ timeout: 10_000 });
  await expect(menu.getByTestId('account-menu-version')).toBeVisible();
}

test('@visual shell-account-menu-member', async ({ page }) => {
  await openAccountMenuOnChat(page);
  await expect(page.getByTestId('shell-account-admin-portal')).toHaveCount(0);
  await shoot(page, 'shell-account-menu-member');
});

test.describe('administrator', () => {
  test.use({ storageState: STORAGE_STATE.admin });

  test('@visual shell-account-menu-admin', async ({ page }) => {
    await openAccountMenuOnChat(page);
    // Gated by GET /api/v2/auth/permissions/administration, so it lands after the menu.
    await expect(page.getByTestId('shell-account-admin-portal')).toBeVisible({ timeout: 10_000 });
    await shoot(page, 'shell-account-menu-admin');
  });
});
