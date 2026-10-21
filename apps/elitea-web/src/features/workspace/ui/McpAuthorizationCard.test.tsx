import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { ApprovalRequestPayload } from '@/shared/desktop/workspaceIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ApprovalDialog } from './ApprovalDialog';
import { McpAuthorizationCard } from './McpAuthorizationCard';

const REQUEST: ApprovalRequestPayload = {
  request_id: 'r1',
  kind: 'mcp_authorization',
  call_id: 'c1',
  tool: 'create_issue',
  toolkit: 'Jira',
  title: 'Jira needs your sign-in',
  detail: 'https://mcp.example.com/mcp',
  server_url: 'https://mcp.example.com/mcp',
  authorize_path: '/app/1/mcps/all/7',
  reason: '',
  can_remember: false,
};

describe('McpAuthorizationCard', () => {
  it('offers the sign-in page first, then waits for Continue', async () => {
    const respond = vi.fn();
    const user = userEvent.setup();
    renderWithTheme(<McpAuthorizationCard request={REQUEST} respond={respond} />);

    expect(screen.getByText('Jira needs your sign-in')).toBeInTheDocument();
    expect(screen.getByText('https://mcp.example.com/mcp')).toBeInTheDocument();
    const authorize = screen.getByRole('link', { name: 'Authorize in browser' });
    expect(authorize).toHaveAttribute('href', '/app/1/mcps/all/7');
    expect(authorize).toHaveAttribute('target', '_blank');
    expect(screen.getByText(/the agent waits here/)).toBeInTheDocument();

    authorize.addEventListener('click', (event) => event.preventDefault());
    await user.click(authorize);
    expect(screen.getByText('Finish signing in in your browser, then continue.')).toBeInTheDocument();
    expect(respond).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(respond.mock.calls).toEqual([['continue'], ['cancel']]);
  });

  it('without a safe sign-in page it says where to sign in and offers no link', () => {
    const { authorize_path: _path, ...withoutPath } = REQUEST;
    const requests: ApprovalRequestPayload[] = [
      withoutPath,
      { ...REQUEST, authorize_path: 'https://evil.example/app/1/mcps/all/7' },
      { ...REQUEST, authorize_path: '/app/1/mcps/all/7/../../x' },
    ];
    for (const request of requests) {
      const { unmount } = renderWithTheme(<McpAuthorizationCard request={request} respond={vi.fn()} />);
      expect(screen.queryByRole('link', { name: 'Authorize in browser' })).toBeNull();
      expect(screen.getByText('Sign in to Jira in the Elitea web app, then continue.')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Continue' })).toBeInTheDocument();
      unmount();
    }
  });

  it('is the card ApprovalDialog shows for an mcp_authorization request', async () => {
    const onRespond = vi.fn();
    const user = userEvent.setup();
    renderWithTheme(<ApprovalDialog request={REQUEST} queued={0} onRespond={onRespond} />);
    expect(screen.getByTestId('approval-mcp-authorization-card')).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    expect(onRespond).toHaveBeenCalledWith('r1', 'continue');
  });
});
