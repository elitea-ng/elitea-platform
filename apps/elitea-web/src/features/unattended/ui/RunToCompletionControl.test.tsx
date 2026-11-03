import { useState } from 'react';

import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { defaultChoice, type RunChoice } from '../model/runChoice';
import { RunToCompletionControl, type RunToCompletionControlProps } from './RunToCompletionControl';

/** The control under a parent that keeps its value; `last` is the last value it sent. */
function setup(props: Partial<Omit<RunToCompletionControlProps, 'value' | 'onChange'>> = {}) {
  const seen: RunChoice[] = [];
  function Host(): React.JSX.Element {
    const [value, setValue] = useState(defaultChoice());
    return (
      <RunToCompletionControl
        scope="desktop"
        testId="run"
        {...props}
        value={value}
        onChange={(next) => {
          seen.push(next);
          setValue(next);
        }}
      />
    );
  }
  renderWithTheme(<Host />);
  return { last: () => seen[seen.length - 1], user: userEvent.setup() };
}

describe('RunToCompletionControl', () => {
  it('is on by default, and the chip turns it off and on', async () => {
    const { last, user } = setup();
    const toggle = screen.getByRole('button', { name: 'Run to completion' });
    expect(toggle).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('run')).toHaveAttribute('data-tone', 'on');
    await user.click(toggle);
    expect(last()?.on).toBe(false);
    expect(toggle).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getByTestId('run')).toHaveAttribute('data-tone', 'off');
  });

  it('shows the desktop parts that answer for the person off, and tints the chip when one is on', async () => {
    const { last, user } = setup();
    await user.click(screen.getByRole('button', { name: 'Run to completion settings' }));
    const dialog = screen.getByRole('dialog', { name: 'Run to completion' });
    expect(within(dialog).getByRole('switch', { name: 'Continue until the goal is met' })).toBeChecked();
    expect(within(dialog).getByTestId('unattended-chain-line')).toHaveTextContent('up to 20 continuations, the spend cap of one run × 10, 12 h');
    expect(within(dialog).getByRole('checkbox', { name: 'Hook asks' })).not.toBeChecked();
    expect(within(dialog).getByRole('checkbox', { name: 'Remote confirmations' })).not.toBeChecked();
    expect(within(dialog).queryByTestId('unattended-sensitive-tools')).toBeNull();
    await user.click(within(dialog).getByRole('checkbox', { name: 'Hook asks' }));
    expect(last()?.grant.hook_asks).toBe(true);
    expect(screen.getByTestId('run')).toHaveAttribute('data-tone', 'strong');
  });

  it('edits the chain limits, and shows a limit that is not valid', async () => {
    const { last, user } = setup();
    await user.click(screen.getByRole('button', { name: 'Run to completion settings' }));
    const dialog = screen.getByRole('dialog', { name: 'Run to completion' });
    await user.click(within(dialog).getByRole('button', { name: 'Change limits' }));
    await user.type(within(dialog).getByTestId('unattended-chain-max-continuations'), '5.5');
    expect(screen.getByTestId('run')).toHaveAttribute('data-tone', 'invalid');
    await user.clear(within(dialog).getByTestId('unattended-chain-max-continuations'));
    await user.type(within(dialog).getByTestId('unattended-chain-max-continuations'), '5');
    expect(last()?.grant.chain.max_continuations).toBe('5');
    expect(within(dialog).getByTestId('unattended-chain-line')).toHaveTextContent('up to 5 continuations');
  });

  it('shows the sensitive tools for the cloud, not the desktop parts', async () => {
    const { last, user } = setup({ scope: 'cloud', sensitiveTools: [{ toolkit: 'github', tool: 'delete_branch' }] });
    await user.click(screen.getByRole('button', { name: 'Run to completion settings' }));
    const dialog = screen.getByRole('dialog', { name: 'Run to completion' });
    expect(within(dialog).queryByRole('checkbox', { name: 'Hook asks' })).toBeNull();
    await user.click(within(dialog).getByRole('checkbox', { name: 'github/delete_branch' }));
    expect(last()?.grant.sensitive_tools).toEqual([{ toolkit: 'github', tool: 'delete_branch' }]);
    expect(screen.getByTestId('run')).toHaveAttribute('data-tone', 'strong');
  });

  it('does not apply while notApplied says why (Plan mode)', () => {
    setup({ notApplied: 'Plan mode stops at the plan.' });
    expect(screen.getByRole('button', { name: 'Run to completion' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Run to completion' })).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getByTestId('run')).toHaveAttribute('data-tone', 'off');
  });
});
