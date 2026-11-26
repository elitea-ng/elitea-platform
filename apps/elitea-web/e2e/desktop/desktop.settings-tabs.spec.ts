/**
 * Settings' desktop rows open their pages (#3078) on the desktop shell's dev harness. The six desktop tabs ride on the
 * `/settings/$tab` catch-all, and the unknown-tab redirect used to send each click straight back to Project general.
 */
import { expect, test } from '@playwright/test';

// A cold Vite server compiles the whole shell on the first load.
test.setTimeout(180_000);

const ROWS = ['Approval rules', 'MCP servers', 'Hooks', 'Computer use', 'Trusted folders', 'Troubleshoot'] as const;

test('each desktop row of Settings opens its page and stays there', async ({ page }) => {
  // The harness has no project to show Project general for: it opens on a desktop tab, then walks every row.
  await page.goto('/?harness&at=/settings/trusted-folders');
  const first = page.getByRole('button', { name: ROWS[0], exact: true });
  await expect(first).toBeVisible({ timeout: 150_000 });

  for (const row of ROWS) {
    await page.getByRole('button', { name: row, exact: true }).click();
    const heading = page.getByRole('heading', { level: 1, name: row, exact: true });
    await expect(heading).toBeVisible();
    // Not bounced a moment later (the redirect ran after the page had drawn).
    await page.waitForTimeout(300);
    await expect(heading).toBeVisible();
  }
});
