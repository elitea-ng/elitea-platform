/**
 * Journey: a non-admin who opens `/admin/app` sees a 403, not the console
 * (smoke finding — the admin router used to mount unconditionally:
 * `services/elitea-main/internal/api/adminui/handler.go`'s `ServeSPA` sits on
 * the root mux with no auth middleware, so it served the SPA shell to anyone,
 * and `pages/admin/router.tsx` rendered the route tree for anyone it was
 * served to. Every WRITE still came back 403 from the server — this journey
 * is about what the PAGE itself said, which used to be nothing).
 *
 * `e2e-member@autotest.local` (`STORAGE_STATE.member`) is the harness's
 * non-admin persona: `scripts/e2e-stack.sh` gives it project-editor scope
 * only, never an administration-mode role, so it is the caller
 * `hasAnyAdminNavAccess()` in `pages/admin/adminNavItems.ts` must refuse.
 */
import { test as memberTest, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

memberTest.use({ storageState: STORAGE_STATE.member });

memberTest('a non-admin opening /admin/app sees the 403, and the router never mounts', async ({ page }) => {
  const response = await page.goto(BASE_URL + '/admin/app/', { waitUntil: 'domcontentloaded' });
  expect(response?.status(), 'the admin SPA is still served at its root — the gate is client-side').toBeLessThan(
    400,
  );

  await expect(page.getByTestId('admin-access-denied')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole('heading', { name: 'Access denied' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Back to the app' })).toHaveAttribute('href', '/app/');

  // The router itself must never mount: no nav, no Users heading, no chunk
  // for any of the eleven admin pages.
  await expect(page.getByTestId('admin-nav')).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'Users' })).toHaveCount(0);

  // A typed deep link is refused the same way, not just the landing page —
  // the gate sits above the route tree, not inside one route of it.
  await page.goto(BASE_URL + '/admin/app/users', { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('admin-access-denied')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('admin-nav')).toHaveCount(0);

  await checkA11y(page);
});
