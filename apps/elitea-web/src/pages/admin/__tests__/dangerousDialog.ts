/**
 * Test helper: fill and confirm the one dangerous-action dialog
 * (`AdminDangerousActionDialog`, ADR-0059 R3.8b, issue 2614).
 */
import { screen, within } from '@testing-library/react';
import type { UserEvent } from '@testing-library/user-event';

/** Choose `code`, type `text` and the target's name, and confirm. */
export async function confirmDangerousAction(
  user: UserEvent,
  target: string,
  { code = 'incident', text = 'ticket 42' }: { code?: string; text?: string } = {},
): Promise<void> {
  const dialog = await screen.findByTestId('admin-dangerous-dialog');
  await user.selectOptions(within(dialog).getByTestId('admin-dangerous-reason-code'), code);
  if (text !== '') await user.type(within(dialog).getByTestId('admin-dangerous-reason-text'), text);
  await user.type(within(dialog).getByTestId('admin-dangerous-target'), target);
  await user.click(within(dialog).getByTestId('admin-dangerous-confirm'));
}
