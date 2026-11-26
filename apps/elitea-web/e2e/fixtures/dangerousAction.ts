/**
 * Fill and confirm the one dangerous-action dialog of the admin console
 * (`AdminDangerousActionDialog`, ADR-0059 R3.8b, issue 2614): a reason code,
 * a short reason, and the typed name of the target.
 */
import { expect, type Page } from '@playwright/test';

export async function confirmDangerousAction(
  page: Page,
  target: string,
  { code = 'maintenance', text = 'e2e journey' }: { code?: string; text?: string } = {},
): Promise<void> {
  const dialog = page.getByTestId('admin-dangerous-dialog');
  await expect(dialog).toBeVisible();
  await dialog.getByTestId('admin-dangerous-reason-code').selectOption(code);
  if (text !== '') await dialog.getByTestId('admin-dangerous-reason-text').fill(text);
  await dialog.getByTestId('admin-dangerous-target').fill(target);
  await dialog.getByTestId('admin-dangerous-confirm').click();
}
