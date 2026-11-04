import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { ChatUnattendedRequest } from '@/shared/api/generated/model';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ThreadGrantField } from './ThreadGrantField';

function setup(stored?: ChatUnattendedRequest, refusal?: unknown) {
  const onChange = vi.fn();
  renderWithTheme(<ThreadGrantField stored={stored} onChange={onChange} refusal={refusal} />);
  return { onChange, user: userEvent.setup() };
}

async function openDetails(user: ReturnType<typeof userEvent.setup>): Promise<HTMLElement> {
  await user.click(screen.getByRole('button', { name: 'Run to completion settings' }));
  return screen.getByRole('dialog', { name: 'Run to completion' });
}

describe('ThreadGrantField', () => {
  it('shows the continue parts, the chain limits and the goal check, and no part that answers for a person', async () => {
    const { onChange, user } = setup();
    const dialog = await openDetails(user);
    expect(within(dialog).getByRole('switch', { name: 'Continue until the goal is met' })).toBeChecked();
    expect(within(dialog).getByRole('switch', { name: 'Check the goal before finishing' })).toBeChecked();
    expect(within(dialog).queryByText('Also answer for me (off by default)')).toBeNull();
    expect(within(dialog).queryByRole('checkbox', { name: 'Hook asks' })).toBeNull();
    expect(within(dialog).queryByRole('checkbox', { name: 'Remote confirmations' })).toBeNull();
    expect(within(dialog).queryByTestId('unattended-sensitive-tools')).toBeNull();
    expect(onChange).not.toHaveBeenCalled();
  });

  it('sends a grant without the opt-ins after an edit', async () => {
    const { onChange, user } = setup();
    const dialog = await openDetails(user);
    await user.click(within(dialog).getByRole('button', { name: 'Change limits' }));
    await user.type(within(dialog).getByTestId('unattended-chain-max-continuations'), '5');
    expect(onChange).toHaveBeenLastCalledWith({
      kind: 'set',
      grant: { continue_step_limit: true, continue_output_limit: true, goal_check: true, chain: { max_continuations: 5 } },
    });
    const sent = onChange.mock.lastCall?.[0] as { grant: Record<string, unknown> };
    expect(Object.keys(sent.grant)).not.toContain('sensitive_tools');
    expect(Object.keys(sent.grant)).not.toContain('hook_asks');
    expect(Object.keys(sent.grant)).not.toContain('remote_confirmations');
    expect(Object.keys(sent.grant)).not.toContain('continue_tool_call_cap');
  });

  it('reports chain limits that are not valid', async () => {
    const { onChange, user } = setup();
    const dialog = await openDetails(user);
    await user.click(within(dialog).getByRole('button', { name: 'Change limits' }));
    await user.type(within(dialog).getByTestId('unattended-chain-max-continuations'), '5.5');
    expect(onChange).toHaveBeenLastCalledWith({ kind: 'invalid' });
  });

  it('seeds from the stored grant, and the chip turns continuing off', async () => {
    const { onChange, user } = setup({ continue_step_limit: true, continue_output_limit: true, goal_check: true, chain: { max_continuations: 5 } });
    expect(screen.getByRole('button', { name: 'Run to completion' })).toHaveAttribute('aria-pressed', 'true');
    const dialog = await openDetails(user);
    expect(within(dialog).getByTestId('unattended-chain-line')).toHaveTextContent('up to 5 continuations');
    await user.keyboard('{Escape}');
    await user.click(screen.getByRole('button', { name: 'Run to completion' }));
    expect(onChange).toHaveBeenLastCalledWith({ kind: 'set', grant: { continue_step_limit: false, continue_output_limit: false, goal_check: true } });
  });

  it('shows Main’s refusal of the grant as text', () => {
    setup(undefined, { error: 'unattended_grant_refused', reason: 'over_maximum', field: 'chain.max_continuations' });
    expect(screen.getByTestId('thread-target-grant-error')).toHaveTextContent(
      'The unattended grant was refused. Most automatic continuations: the value is more than the platform maximum. Type a lower value. Change the grant and send again.',
    );
  });
});
