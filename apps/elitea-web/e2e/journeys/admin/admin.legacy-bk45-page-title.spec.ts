/**
 * elitea_issues: #4406 — Missing Elitea favicon and static tab title on Admin Portal pages.
 *
 * The favicon HALF is already fixed: `src/entries/admin/index.html` links
 * `/assets/favicon.svg`. The TITLE half is not: that same file hardcodes
 * `<title>Elitea Admin</title>` and nothing in `src/entries/admin` or the
 * admin route tree updates `document.title` per section — grepped for
 * `document.title`/`useTitle`/a `title:` route-meta field across
 * `src/entries/admin`, `src/pages/admin`; none exists. Navigating between
 * Users/Roles/Projects therefore leaves the tab reading the same generic
 * "Elitea Admin" the issue complains about, with no per-page context.
 *
 * Marked `test.fail` per this package's rules (product gap, not small to
 * fix — it needs a title convention wired into every admin route, not a
 * one-line change). See S/issues/gaps.md.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

test('#4406: product gap — the admin tab title changes per section (Users vs Roles)', async ({ page }) => {
  test.fail(true, '#4406: product gap — document.title is a static "Elitea Admin" on every admin route; no per-section title wiring exists in src/entries/admin');

  await page.goto(BASE_URL + '/admin/app/users', { waitUntil: 'domcontentloaded' });
  const usersTitle = await page.title();

  await page.goto(BASE_URL + '/admin/app/roles', { waitUntil: 'domcontentloaded' });
  const rolesTitle = await page.title();

  expect(rolesTitle, 'the tab title should reflect the current admin section').not.toBe(usersTitle);
  expect(usersTitle).toBe('Users - Elitea Admin');
  expect(rolesTitle).toBe('Roles - Elitea Admin');
});
