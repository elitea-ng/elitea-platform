import { useState } from 'react';

import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { defaultGrant, type UnattendedGrantValue } from '../model/grant';
import { UnattendedGrantForm, type UnattendedGrantFormProps } from './UnattendedGrantForm';
import { UnattendedGrantSummary } from './UnattendedGrantSummary';

/** The form under a parent that keeps its value; `seen` holds every value it sent. */
function setup(props: Omit<UnattendedGrantFormProps, 'value' | 'onChange'> = {}) {
  const seen: UnattendedGrantValue[] = [];
  function Host(): React.JSX.Element {
    const [value, setValue] = useState(defaultGrant());
    return (
      <UnattendedGrantForm
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
  return { seen, last: () => seen[seen.length - 1], user: userEvent.setup() };
}

describe('UnattendedGrantForm', () => {
  it('shows each desktop part with its help, and toggles it', async () => {
    const { last, user } = setup();
    expect(screen.getByRole('switch', { name: 'Continue at the step limit' })).toBeChecked();
    expect(screen.getByRole('switch', { name: 'Continue after 512 tool calls' })).toBeChecked();
    expect(screen.getByRole('switch', { name: 'Allow hook questions' })).not.toBeChecked();
    expect(screen.getByRole('switch', { name: 'Approve remote tool confirmations' })).not.toBeChecked();
    expect(screen.queryByRole('switch', { name: 'Continue at the output limit' })).toBeNull();
    expect(screen.getByText(/Without the grant: the run asks; on timeout, it stops\./u)).toBeInTheDocument();
    await user.click(screen.getByRole('switch', { name: 'Allow hook questions' }));
    expect(last()?.hook_asks).toBe(true);
    await user.click(screen.getByRole('switch', { name: 'Continue at the step limit' }));
    expect(last()?.continue_step_limit).toBe(false);
  });

  it('hides the desktop-only parts, and shows the output limit for the cloud', () => {
    setup({ desktop: false, cloud: true });
    expect(screen.getByRole('switch', { name: 'Continue at the step limit' })).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Continue at the output limit' })).toBeChecked();
    for (const name of ['Continue after 512 tool calls', 'Allow hook questions', 'Approve remote tool confirmations']) expect(screen.queryByRole('switch', { name })).toBeNull();
  });

  it('lists the sensitive tools only when they are given, and names the picked ones', async () => {
    const { last, user } = setup({ sensitiveTools: [{ toolkit: 'github', tool: 'delete_repo', label: 'Delete a repository' }, { toolkit: 'jira', tool: 'delete_issue' }] });
    await user.click(screen.getByRole('checkbox', { name: 'Delete a repository' }));
    await user.click(screen.getByRole('checkbox', { name: 'jira/delete_issue' }));
    expect(last()?.sensitive_tools).toEqual([
      { toolkit: 'github', tool: 'delete_repo' },
      { toolkit: 'jira', tool: 'delete_issue' },
    ]);
    await user.click(screen.getByRole('checkbox', { name: 'Delete a repository' }));
    expect(last()?.sensitive_tools).toEqual([{ toolkit: 'jira', tool: 'delete_issue' }]);
  });

  it('shows no sensitive tools without the prop', () => {
    setup();
    expect(screen.queryByTestId('unattended-sensitive-tools')).toBeNull();
  });

  it('says which chain limit is not valid', async () => {
    const { last, user } = setup();
    const count = screen.getByTestId('unattended-chain-max-continuations');
    expect(screen.getByText('A whole number from 0. The platform sets the maximum. Default: 20.')).toBeInTheDocument();
    await user.type(count, '5.5');
    expect(last()?.chain.max_continuations).toBe('5.5');
    expect(screen.getByText('Type a whole number from 0.')).toBeInTheDocument();
    expect(count).toHaveAttribute('aria-invalid', 'true');
    await user.type(screen.getByTestId('unattended-chain-max-spend'), '1.23456');
    expect(screen.getByText('Type an amount more than 0, with at most 4 decimals.')).toBeInTheDocument();
    await user.clear(count);
    expect(screen.queryByText('Type a whole number from 0.')).toBeNull();
  });
});

describe('UnattendedGrantSummary', () => {
  it('lists what the grant answers, what it does not, and the chain limits', () => {
    renderWithTheme(<UnattendedGrantSummary value={{ ...defaultGrant(), hook_asks: true, chain: { max_continuations: '5', max_spend: '', deadline_hours: '2' } }} />);
    expect(screen.getByTestId('unattended-summary-answers')).toHaveTextContent('Answers: Continue at the step limit, Continue after 512 tool calls, Allow hook questions');
    expect(screen.getByTestId('unattended-summary-left')).toHaveTextContent('Approve remote tool confirmations');
    expect(screen.getByTestId('unattended-summary-chain')).toHaveTextContent('Most automatic continuations: 5, Deadline (hours): 2');
  });

  it('says the platform defaults apply when no limit is typed', () => {
    renderWithTheme(<UnattendedGrantSummary value={defaultGrant()} />);
    expect(screen.getByTestId('unattended-summary-chain')).toHaveTextContent('platform defaults');
    expect(screen.queryByText(/Continue at the output limit/u)).toBeNull();
  });
});
