/**
 * Settings › Trusted folders (ADR-0061 decision 5i, #3062) on the desktop shell's dev harness: the folders the host
 * trusts (`trust_list`), with their state, and Revoke trust, which tells the host and drops the row. The host's own
 * side (the trust store, stopping running commands) is the desktop crate's tests.
 */
import { expect, test } from '@playwright/test';

interface HarnessWindow {
  __harness: { trust: { revoked: string[] } };
}

// A cold Vite server compiles the whole shell on the first load.
test.setTimeout(180_000);

test('lists the trusted folders and revokes one', async ({ page }) => {
  await page.goto('/?harness&at=/settings/trusted-folders');
  await expect(page.getByRole('heading', { name: 'Trusted folders' })).toBeVisible({ timeout: 150_000 });

  const table = page.getByTestId('trusted-folders');
  const trusted = table.getByTestId('trusted-folder-w1');
  const paused = table.getByTestId('trusted-folder-w2');
  await expect(trusted).toContainText('elitea-platform');
  await expect(trusted).toContainText('No sandbox');
  await expect(paused).toContainText('docs');
  await expect(paused).not.toContainText('No sandbox');

  await trusted.getByRole('button', { name: 'Revoke trust' }).click();
  await expect.poll(() => page.evaluate(() => (window as unknown as HarnessWindow).__harness.trust.revoked)).toEqual(['w1']);
  await expect(trusted).toHaveCount(0);
  await expect(paused).toBeVisible();
});
