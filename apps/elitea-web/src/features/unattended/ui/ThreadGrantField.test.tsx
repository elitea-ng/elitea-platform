import { useState } from 'react';

import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { ThreadGrantDraft } from '@/entities/thread-target';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ThreadGrantField } from './ThreadGrantField';

/** The field under a parent that keeps the draft, as the thread target form does. */
function setup(initial: ThreadGrantDraft = { kind: 'default' }, refusal?: unknown) {
  const onChange = vi.fn<(next: ThreadGrantDraft) => void>();
  function Host(): React.JSX.Element {
    const [value, setValue] = useState(initial);
    return (
      <ThreadGrantField
        value={value}
        onChange={next => {
          onChange(next);
          setValue(next);
        }}
        refusal={refusal}
      />
    );
  }
  renderWithTheme(<Host />);
  return { onChange, user: userEvent.setup() };
}

async function openDetails(user: ReturnType<typeof userEvent.setup>): Promise<HTMLElement> {
  await user.click(screen.getByRole('button', { name: 'Run to completion settings' }));
  return screen.getByRole('dialog', { name: 'Run to completion' });
}

const STORED: ThreadGrantDraft = { kind: 'set', grant: { continue_step_limit: true, continue_output_limit: true, goal_check: true, chain: { max_continuations: 5 } } };

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
    const sent = onChange.mock.lastCall?.[0];
    expect(sent?.kind).toBe('set');
    const grant = sent?.kind === 'set' ? sent.grant : {};
    expect(grant).toEqual({ continue_step_limit: true, continue_output_limit: true, goal_check: true, chain: { max_continuations: 5 } });
    for (const part of ['sensitive_tools', 'hook_asks', 'remote_confirmations', 'continue_tool_call_cap']) expect(Object.keys(grant)).not.toContain(part);
  });

  it('says that chain limits are not valid, under the field and on the closed chip', async () => {
    const { onChange, user } = setup();
    const dialog = await openDetails(user);
    await user.click(within(dialog).getByRole('button', { name: 'Change limits' }));
    await user.type(within(dialog).getByTestId('unattended-chain-max-continuations'), '5.5');
    expect(onChange.mock.lastCall?.[0].kind).toBe('invalid');
    await user.keyboard('{Escape}');
    expect(screen.getByTestId('thread-target-grant-invalid')).toHaveTextContent('A chain limit is not valid. Open the settings and correct it.');
    expect(screen.getByTestId('thread-target-grant')).toHaveAttribute('data-tone', 'invalid');
  });

  it('shows the stored grant, and the chip turns continuing off', async () => {
    const { onChange, user } = setup(STORED);
    expect(screen.getByRole('button', { name: 'Run to completion' })).toHaveAttribute('aria-pressed', 'true');
    const dialog = await openDetails(user);
    expect(within(dialog).getByTestId('unattended-chain-line')).toHaveTextContent('up to 5 continuations');
    await user.keyboard('{Escape}');
    await user.click(screen.getByRole('button', { name: 'Run to completion' }));
    expect(onChange.mock.lastCall?.[0]).toMatchObject({ kind: 'set', grant: { continue_step_limit: false, continue_output_limit: false, goal_check: true } });
  });

  it('is controlled: a new value from the form replaces what it shows', async () => {
    const user = userEvent.setup();
    const { rerender } = renderWithTheme(<ThreadGrantField value={STORED} onChange={vi.fn()} />);
    rerender(<ThreadGrantField value={{ kind: 'default' }} onChange={vi.fn()} />);
    const dialog = await openDetails(user);
    expect(within(dialog).getByTestId('unattended-chain-line')).toHaveTextContent('up to 20 continuations');
  });

  it('shows Main’s refusal of the grant as text', () => {
    setup(undefined, { error: 'unattended_grant_refused', reason: 'over_maximum', field: 'chain.max_continuations' });
    expect(screen.getByTestId('thread-target-grant-error')).toHaveTextContent(
      'The unattended grant was refused. Most automatic continuations: the value is more than the platform maximum. Type a lower value. Change the grant and send again.',
    );
  });
});
