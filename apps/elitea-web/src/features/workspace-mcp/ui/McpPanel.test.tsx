/**
 * The MCP settings panel (#1230): both files with each server's status and
 * output, the add form validated before a save, and the policy's word.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { McpIpc, McpList } from '@/shared/desktop/mcpIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { McpPanel } from './McpPanel';

const LIST: McpList = {
  allowed: true,
  user: { path: '/config/mcp.json', servers: { github: { command: '/opt/bin/github-mcp', args: ['stdio'], network: true } } },
  workspace: { path: '/data/workspaces/w1/mcp.json', servers: {} },
  status: [{ name: 'github', scope: 'user', state: 'failed', tools: [], error: 'it exited during start', stderr_tail: 'panic: no token', network: true }],
  proposals: [],
};

function setup(list: McpList = LIST) {
  const mocks = {
    list: vi.fn(() => Promise.resolve(list)),
    save: vi.fn(() => Promise.resolve()),
    remove: vi.fn(() => Promise.resolve(true)),
    restart: vi.fn(() => Promise.resolve(list.status[0]!)),
    answerProposal: vi.fn(() => Promise.resolve({ accepted: [] })),
    onProposal: vi.fn(() => Promise.resolve(() => undefined)),
  };
  const ipc: McpIpc = mocks;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <McpPanel ipc={ipc} workspaceId="w1" />
    </QueryClientProvider>,
  );
  return mocks;
}

describe('McpPanel', { timeout: 20_000 }, () => {
  it('lists the servers with their status and output', async () => {
    const ipc = setup();
    const row = await screen.findByTestId('mcp-server-user-github');
    expect(row).toHaveTextContent('Did not start');
    expect(row).toHaveTextContent('/opt/bin/github-mcp stdio');
    expect(row).toHaveTextContent('it exited during start');
    await userEvent.click(screen.getByRole('button', { name: 'Show its output' }));
    expect(screen.getByTestId('mcp-stderr-github')).toHaveTextContent('panic: no token');
    await userEvent.click(screen.getByRole('button', { name: 'Restart' }));
    await waitFor(() => expect(ipc.restart).toHaveBeenCalledWith('w1', 'github'));
    expect(screen.queryByTestId('mcp-not-allowed')).not.toBeInTheDocument();
  });

  it('saves a new server only once the form is valid', async () => {
    const ipc = setup();
    await userEvent.click(await screen.findByRole('button', { name: 'Add a server' }));
    await userEvent.type(screen.getByLabelText('Name'), 'fs');
    await userEvent.type(screen.getByLabelText('Program'), 'npx');
    expect(screen.getByText('The full path of the program, starting with /.')).toBeInTheDocument();
    const save = screen.getByRole('button', { name: 'Save' });
    expect(save).toBeDisabled();
    await userEvent.clear(screen.getByLabelText('Program'));
    await userEvent.type(screen.getByLabelText('Program'), '/usr/local/bin/fs-mcp');
    await userEvent.click(save);
    await waitFor(() =>
      expect(ipc.save).toHaveBeenCalledWith(
        'user',
        'fs',
        expect.objectContaining({ command: '/usr/local/bin/fs-mcp', network: false, allowed_tools: null }),
        'w1',
      ),
    );
  });

  it('says when the policy runs none', async () => {
    setup({ ...LIST, allowed: false });
    expect(await screen.findByTestId('mcp-not-allowed')).toBeInTheDocument();
  });
});
