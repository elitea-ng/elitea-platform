/**
 * Shell: the account menu at the rail's foot (PR 1553), the same one the
 * desktop app has (`widgets/account-menu`).
 *
 * - AM01: it holds the account places that left the rail (Settings,
 *   Credentials, the Catalog, help) and the version line; a member sees no
 *   admin portal.
 * - AM02: an administrator gets "Admin portal", which opens the console, and
 *   the console's own account menu leads back with "Back to Elitea".
 * - AM03: on the collapsed rail the avatar alone opens the same menu.
 */
import { expect, test, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { keepThemeOffTheServer } from '../../fixtures/serverTheme';

const COLLAPSED_KEY = 'el.sidebar.collapsed';

async function seedCollapsed(page: Page, collapsed: boolean): Promise<void> {
  await page.addInitScript(
    ([key, value]) => {
      try {
        window.localStorage.setItem(key, value);
      } catch {
        // A browser that refuses storage still renders the default.
      }
    },
    [COLLAPSED_KEY, collapsed ? '1' : '0'] as const,
  );
}

async function openAccountMenu(page: Page) {
  await page.getByTestId('shell-account').click();
  const menu = page.getByRole('menu', { name: 'Account menu' });
  await expect(menu).toBeVisible({ timeout: 10_000 });
  return menu;
}

test.beforeEach(async ({ page }) => {
  await keepThemeOffTheServer(page);
});

test.describe('member', () => {
  test.use({ storageState: STORAGE_STATE.member });

  test('AM01: the account menu holds the account places and the version, and no admin portal', async ({ page }) => {
    await seedCollapsed(page, false);
    await page.goto(BASE_URL + '/app/chat', { waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

    const menu = await openAccountMenu(page);
    for (const name of ['Settings', 'Credentials', 'Catalog', 'Get help', 'Sign out']) {
      await expect(menu.getByRole('menuitem', { name, exact: true })).toBeVisible();
    }
    await expect(page.getByTestId('account-menu-version')).toContainText(/Version \S+/);
    await expect(page.getByTestId('shell-account-admin-portal')).toHaveCount(0);
    await checkA11y(page);

    await menu.getByRole('menuitem', { name: 'Credentials', exact: true }).click();
    await expect(page).toHaveURL(/\/app\/credentials/, { timeout: 15_000 });
  });

  test('AM03: on the collapsed rail the avatar opens the same menu', async ({ page }) => {
    await seedCollapsed(page, true);
    await page.goto(BASE_URL + '/app/chat', { waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

    const menu = await openAccountMenu(page);
    await expect(menu.getByRole('menuitem', { name: 'Settings', exact: true })).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(menu).toBeHidden();
    // Focus goes back to the button that opened it.
    await expect(page.getByTestId('shell-account')).toBeFocused();
  });
});

test.describe('administrator', () => {
  test.use({ storageState: STORAGE_STATE.admin });

  test('AM02: "Admin portal" opens the console, and "Back to Elitea" returns', async ({ page }) => {
    await seedCollapsed(page, false);
    await page.goto(BASE_URL + '/app/chat', { waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

    await openAccountMenu(page);
    const portal = page.getByTestId('shell-account-admin-portal');
    await expect(portal).toBeVisible({ timeout: 10_000 });
    await expect(portal).toHaveAttribute('href', '/admin/app/');
    await portal.click();

    await expect(page).toHaveURL(/\/admin\/app\//, { timeout: 15_000 });
    await expect(page.getByTestId('admin-nav')).toBeVisible({ timeout: 20_000 });
    // The console has no theme control of its own: the theme is set in the app.
    await expect(page.getByRole('group', { name: 'Theme' })).toHaveCount(0);

    await page.getByTestId('admin-nav-user-button').click();
    const adminMenu = page.getByRole('menu', { name: 'Account menu' });
    await expect(adminMenu.getByTestId('account-menu-version')).toContainText(/Version \S+/);
    await adminMenu.getByRole('menuitem', { name: 'Back to Elitea' }).click();
    await expect(page).toHaveURL(/\/app\//, { timeout: 15_000 });
    await expect(page.getByTestId('shell-account')).toBeVisible({ timeout: 20_000 });
  });
});
