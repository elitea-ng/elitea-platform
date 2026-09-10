/**
 * Shell: sidebar nav rail — tooltips, labels, navigation, and Settings in
 * dark mode (onetest package w1-sidebar-menu, EliteaAI/elitea_issues#3846).
 *
 * Ported from ELITEA-1021 (main-area tooltip regression), ELITEA-1023
 * (Applications label), ELITEA-1025 (Settings loads in dark mode) and
 * ELITEA-1026 (sidebar navigation functional after the icon change). See
 * `S/port/ledger-P5-mcp-shell.tsv` for the full per-case disposition,
 * including the cases NOT ported here (the malformed "None" case id, the
 * icon-outline case ELITEA-1024, and the Bare-persona personalization cases,
 * which need a real model turn).
 *
 * `SidebarNavItem.tsx` wraps every rail row in a `Tooltip` whose title is the
 * row's own label exactly while the label text is hidden (collapsed rail) —
 * the same mechanism `chat.navigation.spec.ts` already exercises for the
 * "Agents" row alone. This file asserts the EXACT tooltip text for every
 * main-rail row (ELITEA-1021) and the Applications row specifically in both
 * colour schemes (ELITEA-1023), verified live against this stack before
 * writing this file.
 *
 * REAL LABELS, not the case's literal legacy copy: the toolkits row reads
 * "Toolkits & Indexes" (`navSections.ts`), not "Toolkits" — the same
 * "assert what renders" rule `shell.resources.spec.ts`'s header documents
 * for the Help Center rename.
 */
import { expect, test, type Locator, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';

/** `STORAGE_NAMESPACE` + `collapsedPersistence.ts`'s own key (`chat.navigation.spec.ts` uses the same constant). */
const COLLAPSED_KEY = 'el.sidebar.collapsed';

/** Seeds the collapsed flag before the app loads, so there is one path through a test rather than a branch on whatever the last run left. */
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

function rail(page: Page) {
  return page.getByRole('navigation', { name: 'side-bar' });
}

/** Hovers `link`, reads the tooltip text that appears, then moves the mouse away so the next hover starts clean. */
async function tooltipTextOf(page: Page, link: Locator): Promise<string> {
  await link.hover();
  const tooltip = page.getByRole('tooltip');
  await expect(tooltip).toBeVisible({ timeout: 5_000 });
  const text = (await tooltip.first().innerText()).trim();
  await page.mouse.move(400, 400);
  await expect(tooltip).toBeHidden({ timeout: 5_000 });
  return text;
}

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1021 — the main nav rail's 7 items each show their exact
 * label as a right-side tooltip when the rail is collapsed. Verified live:
 * every one of the 7 renders `getByRole('tooltip')` with the row's own text.
 * ──────────────────────────────────────────────────────────────────────── */
test('SB01: every main-rail icon shows its exact label as a tooltip when collapsed', async ({ page }) => {
  await seedCollapsed(page, false);
  await page.goto(BASE_URL + '/app/chat', { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  // The a11y sweep runs EXPANDED, before the collapse below. Collapsed, the
  // sidebar's own "Create" button (`widgets/create-button`'s `isSimple`
  // branch) renders neither a visible label nor an `aria-label` at all — a
  // real, PRE-EXISTING axe violation (`button-name`) this test's tooltip
  // assertions do not touch. That defect is the create-button widget's own —
  // recorded against ELITEA-1046/1047 in `shell.create-button.spec.ts` and
  // `S/port/defects.md`, not repeated here as a second failure of a test
  // whose subject is the NAV ROWS' tooltips, not the create button.
  await checkA11y(page);

  const toggle = page.getByTestId('sidebar-collapse-toggle');
  await toggle.click();
  await expect(toggle).toHaveAccessibleName('Expand sidebar');

  const navRail = rail(page);

  // `navSections()`'s own 7 items this case's steps enumerate — real label
  // for Toolkits ("Toolkits & Indexes"), see the file header.
  const rows: readonly string[] = ['Chats', 'Agents', 'Pipelines', 'Credentials', 'Toolkits & Indexes', 'MCPs', 'Artifacts'];

  for (const label of rows) {
    const link = navRail.getByRole('link', { name: label, exact: true });
    await expect(link, `the ${label} row must exist exactly once`).toHaveCount(1);
    const tooltipText = await tooltipTextOf(page, link);
    expect(tooltipText, `${label}'s tooltip must read its own label, not another row's`).toBe(label);
  }

  // Leave the rail as this suite found it.
  await toggle.click();
  await expect(toggle).toHaveAccessibleName('Collapse sidebar');
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1023 — "Applications" reads exactly that, expanded label
 * and collapsed tooltip, in both colour schemes. The app's DEFAULT scheme is
 * dark (`shell.theme-brand.spec.ts`), so this test drives dark first and
 * light second, matching the case's own step order.
 * ──────────────────────────────────────────────────────────────────────── */
test('SB02: the Applications row reads "Applications" — expanded label and collapsed tooltip, dark and light', async ({
  page,
}) => {
  await seedCollapsed(page, false);
  await page.goto(BASE_URL + '/app/apps/applications', { waitUntil: 'domcontentloaded' });

  const navRail = rail(page);
  const appsLink = navRail.getByRole('link', { name: 'Applications', exact: true });

  const assertBothModes = async (): Promise<void> => {
    // Expanded: the visible text label.
    await expect(appsLink).toHaveText('Applications', { timeout: 15_000 });

    // Collapsed: the tooltip carries the same exact text.
    const toggle = page.getByTestId('sidebar-collapse-toggle');
    await toggle.click();
    await expect(toggle).toHaveAccessibleName('Expand sidebar');
    const tooltipText = await tooltipTextOf(page, appsLink);
    expect(tooltipText).toBe('Applications');

    // Back to expanded, for the next colour scheme's own assertion.
    await toggle.click();
    await expect(toggle).toHaveAccessibleName('Collapse sidebar');
    await expect(appsLink).toHaveText('Applications', { timeout: 10_000 });
  };

  // ── dark (default) ──────────────────────────────────────────────────────
  await expect(page.locator('html')).toHaveAttribute('data-el-scheme', 'dark');
  await assertBothModes();

  // ── light ────────────────────────────────────────────────────────────────
  await page.goto(BASE_URL + '/app/settings/personalization', { waitUntil: 'domcontentloaded' });
  await page.getByRole('button', { name: 'Light', exact: true }).click();
  await expect(page.locator('html')).toHaveAttribute('data-el-scheme', 'light');
  await page.goto(BASE_URL + '/app/apps/applications', { waitUntil: 'domcontentloaded' });
  await assertBothModes();

  await checkA11y(page);

  // Leave dark mode as this suite found it — other journeys assume the
  // compiled default.
  await page.goto(BASE_URL + '/app/settings/personalization', { waitUntil: 'domcontentloaded' });
  await page.getByRole('button', { name: 'Dark', exact: true }).click();
  await expect(page.locator('html')).toHaveAttribute('data-el-scheme', 'dark');
});

/**
 * Route error boundary copy (same pattern `shell.deeplink.spec.ts` guards
 * against — issue #132's failure mode: a 200 response that still renders
 * "Something went wrong").
 */
const ERROR_BOUNDARY_TEXT = /something went wrong|unexpected error/i;

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1026 — every main-rail item still navigates to its own
 * page, none broken/missing/unresponsive. Broader than `chat.navigation.
 * spec.ts`'s single Agents click: this walks all 7 rows.
 *
 * The proof is state-independent on purpose: `Toolkits & Indexes` redirects
 * to its OWN create page when the project holds no toolkit yet
 * (`shouldRedirectToCreatePage`, documented in `mcps.oauth.spec.ts`), so a
 * fixed "list must render" assertion would be true or false depending on
 * what OTHER journeys left in the shared project, not on whether this row's
 * navigation works. Every stop is instead checked the same three ways: the
 * URL changed, the clicked row is now the rail's OWN active item (a real
 * `Mui-selected` class MUI applies from the `selected` prop this row's own
 * navigation state computes — `SidebarNavItem.tsx`), the PREVIOUSLY active
 * row is not, and the route did not resolve to the error boundary.
 * ──────────────────────────────────────────────────────────────────────── */
test('SB03: every main-rail item navigates to its own page', async ({ page }) => {
  await seedCollapsed(page, false);
  await page.goto(BASE_URL + '/app/chat', { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  const navRail = rail(page);
  const labels: readonly string[] = ['Agents', 'Pipelines', 'Credentials', 'Toolkits & Indexes', 'MCPs', 'Artifacts', 'Chats'];

  let previous = navRail.getByRole('link', { name: 'Chats', exact: true });
  for (const label of labels) {
    const link = navRail.getByRole('link', { name: label, exact: true });
    await link.click();

    await expect(link, `${label} must become the rail's active item`).toHaveClass(/Mui-selected/, { timeout: 15_000 });
    if (label !== 'Chats') {
      await expect(previous, `the previously active row must lose Mui-selected`).not.toHaveClass(/Mui-selected/);
    }
    await expect(page.getByText(ERROR_BOUNDARY_TEXT)).toHaveCount(0);

    previous = link;
  }

  // Closing the loop: the case's own steps start from Chat, and the last
  // stop above returns there — the composer must be back too.
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 15_000 });

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-1025 — the Settings panel loads fully in dark mode across
 * every sub-section. REAL section labels (`settingsSections.ts`): "AI
 * Providers" not "AI Configuration", and there is no "Monitoring" section at
 * all in this app (only "Analytics"/"Usage", both platform-flag gated) — the
 * case's own "Monitoring" step is therefore not asserted; every OTHER
 * sub-section the case names (Personal Tokens, Secrets, Users) is, plus "AI
 * Providers" standing in for "AI Configuration".
 * ──────────────────────────────────────────────────────────────────────── */
test('SB04: the Settings drawer loads every sub-section fully in dark mode', async ({ page }) => {
  await page.goto(BASE_URL + '/app/settings/personalization', { waitUntil: 'domcontentloaded' });
  await page.getByRole('button', { name: 'Dark', exact: true }).click();
  await expect(page.locator('html')).toHaveAttribute('data-el-scheme', 'dark');

  await page.goto(BASE_URL + '/app/settings/model-configuration', { waitUntil: 'domcontentloaded' });

  // AI Providers (this test's own landing tab): a real, data-bearing control.
  await expect(page.getByText('Request a model connection', { exact: true })).toBeVisible({ timeout: 15_000 });

  // Personal Tokens.
  await page.getByRole('button', { name: 'Personal Tokens', exact: true }).click();
  await expect(page.getByText('TOKEN NAME', { exact: false })).toBeVisible({ timeout: 15_000 });

  // Secrets.
  await page.getByRole('button', { name: 'Secrets', exact: true }).click();
  await expect(page.getByRole('grid')).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText('Rows per page', { exact: true })).toBeVisible();

  // Users (project 1 is not this persona's personal project, so the tab is offered — `settingsSections.ts`'s `isPersonalProject` gate).
  await page.getByRole('button', { name: 'Users', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Invite', exact: true })).toBeVisible({ timeout: 15_000 });

  await checkA11y(page);

  await page.getByRole('button', { name: 'Preferences', exact: true }).click();
  await page.getByRole('button', { name: 'Light', exact: true }).click();
  await expect(page.locator('html')).toHaveAttribute('data-el-scheme', 'light');
  await page.getByRole('button', { name: 'Dark', exact: true }).click();
  await expect(page.locator('html')).toHaveAttribute('data-el-scheme', 'dark');
});
