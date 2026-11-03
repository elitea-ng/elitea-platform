import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { defaultGrant } from '@/features/unattended';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ChatBoxUnattendedControls } from './ChatBoxUnattended';
import { grantForSend, useChatBoxUnattended, type ChatBoxUnattended } from './hooks/useChatBoxUnattended';

const tools = [
  { toolkit: 'github', tool: 'delete_branch' },
  { toolkit: 'jira', tool: 'delete_issue' },
];

function setup() {
  const seen: { current: ChatBoxUnattended | null } = { current: null };
  function Host(): React.JSX.Element {
    const unattended = useChatBoxUnattended('conv-1', tools);
    seen.current = unattended;
    return <ChatBoxUnattendedControls unattended={unattended} disabled={false} />;
  }
  renderWithTheme(<Host />);
  return { seen, user: userEvent.setup() };
}

describe('ChatBoxUnattendedControls', () => {
  it('shows the cloud grant before the mode turns on, and its summary while it is on', async () => {
    const { seen, user } = setup();
    expect(seen.current?.wire()).toBeUndefined();
    await user.click(screen.getByTestId('chat-unattended-open'));
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByRole('switch', { name: 'Continue at the output limit' })).toBeChecked();
    expect(within(dialog).queryByRole('switch', { name: 'Allow hook questions' })).toBeNull();
    await user.click(within(dialog).getByRole('checkbox', { name: 'github/delete_branch' }));
    await user.click(screen.getByTestId('chat-unattended-start'));
    const notice = screen.getByTestId('chat-unattended-notice');
    expect(within(notice).getByTestId('unattended-summary-answers')).toHaveTextContent('Sensitive tools: github/delete_branch');
    const first = seen.current?.wire();
    expect(first).toMatchObject({ continue_step_limit: true, continue_output_limit: true, sensitive_tools: [{ toolkit_name: 'github', tool_name: 'delete_branch' }] });
    // Each send starts a new chain: a new grant id.
    expect(seen.current?.wire()?.['grant_id']).not.toBe(first?.['grant_id']);
    // The closing dialog still hides the page from the accessibility tree for its exit transition.
    await user.click(within(notice).getByText('Turn off'));
    expect(screen.queryByTestId('chat-unattended-notice')).toBeNull();
    expect(seen.current?.wire()).toBeUndefined();
  });
});

describe('grantForSend', () => {
  it('drops a named tool that the agent no longer has, and all of them without an agent', () => {
    const grant = { ...defaultGrant(), sensitive_tools: [{ toolkit: 'github', tool: 'delete_branch' }, { toolkit: 'old', tool: 'gone' }] };
    expect(grantForSend(grant, tools, 'g-1')).toEqual({ grant_id: 'g-1', continue_step_limit: true, continue_output_limit: true, sensitive_tools: [{ toolkit_name: 'github', tool_name: 'delete_branch' }] });
    expect(grantForSend(grant, undefined, 'g-2')).toEqual({ grant_id: 'g-2', continue_step_limit: true, continue_output_limit: true });
    expect(grantForSend(null, tools, 'g-3')).toBeUndefined();
  });
});
