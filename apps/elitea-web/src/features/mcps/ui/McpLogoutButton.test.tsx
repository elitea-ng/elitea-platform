import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { setAccessToken } from '../lib/storage';

import { McpLogoutButton } from './McpLogoutButton';

afterEach(() => {
  window.sessionStorage.clear();
});

describe('McpLogoutButton', () => {
  it('clicking the icon opens the confirmation modal without logging out yet', async () => {
    const user = userEvent.setup();
    setAccessToken('https://mcp.example.com', 'tok', 3600, undefined, undefined, undefined);
    renderWithTheme(<McpLogoutButton serverUrl="https://mcp.example.com" />);

    await user.click(screen.getByRole('button', { name: /log out/i }));

    expect(screen.getByText('Are you sure to log out?')).toBeInTheDocument();
  });

  it('confirming logout clears the token, calls onSuccess, and closes the modal', async () => {
    const user = userEvent.setup();
    const onSuccess = vi.fn();
    setAccessToken('https://mcp.example.com', 'tok', 3600, undefined, undefined, undefined);
    renderWithTheme(
      <McpLogoutButton
        serverUrl="https://mcp.example.com"
        onSuccess={onSuccess}
      />,
    );

    await user.click(screen.getByRole('button', { name: /log out/i }));
    await user.click(screen.getByRole('button', { name: 'Log out' }));

    expect(onSuccess).toHaveBeenCalledTimes(1);
    expect(screen.getByText('You have successfully logged out!')).toBeInTheDocument();
  });

  it('says the user was not logged in when no grant was held', async () => {
    const user = userEvent.setup();
    renderWithTheme(<McpLogoutButton serverUrl="https://mcp.example.com" />);

    await user.click(screen.getByRole('button', { name: /log out/i }));
    await user.click(screen.getByRole('button', { name: 'Log out' }));

    expect(screen.getByText('You were not logged in.')).toBeInTheDocument();
    expect(screen.queryByText('You have successfully logged out!')).not.toBeInTheDocument();
  });

  it('cancelling the modal leaves the token untouched', async () => {
    const user = userEvent.setup();
    setAccessToken('https://mcp.example.com', 'tok', 3600, undefined, undefined, undefined);
    renderWithTheme(<McpLogoutButton serverUrl="https://mcp.example.com" />);

    await user.click(screen.getByRole('button', { name: /log out/i }));
    await user.click(screen.getByRole('button', { name: 'Cancel' }));

    await waitFor(() => expect(screen.queryByText('Are you sure to log out?')).not.toBeInTheDocument());
  });
});
