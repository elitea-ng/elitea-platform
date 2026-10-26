/**
 * The repository proposal card (#1230): it shows what the file proposes
 * (values masked as the host masked them), adds only the servers ticked,
 * "Not now" answers with none, and a server that cannot be added cannot
 * be ticked.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { McpIpc, McpList } from '@/shared/desktop/mcpIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { McpProposals } from './McpProposals';

const LIST: McpList = {
  allowed: true,
  status: [],
  proposals: [
    {
      file: '.mcp.json',
      file_sha256: 'a'.repeat(64),
      servers: [
        { name: 'fs', command: 'npx', args: ['-y', '@modelcontextprotocol/server-filesystem', '.'], env_keys: ['API_TOKEN'], env_preview: { API_TOKEN: 'se… (16 characters)' } },
        { name: 'local', command: './bin/server', args: [], env_keys: [], env_preview: {}, problem: 'its command is a relative path (inside the repository)' },
      ],
    },
  ],
};

function setup(list: McpList = LIST) {
  const answers: string[][] = [];
  let current = list;
  const mocks = {
    answers,
    list: vi.fn(() => Promise.resolve(current)),
    save: vi.fn(() => Promise.resolve()),
    remove: vi.fn(() => Promise.resolve(true)),
    restart: vi.fn(() => Promise.reject(new Error('unused'))),
    answerProposal: vi.fn((_workspaceId: string, _digest: string, accept: string[]) => {
      answers.push(accept);
      current = { ...current, proposals: [] };
      return Promise.resolve({ accepted: accept });
    }),
    onProposal: vi.fn(() => Promise.resolve(() => undefined)),
  };
  const ipc: McpIpc = mocks;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <McpProposals workspaceId="w1" ipc={ipc} />
    </QueryClientProvider>,
  );
  return mocks;
}

describe('McpProposals', { timeout: 20_000 }, () => {
  it('shows the proposal and adds only the ticked servers', async () => {
    const ipc = setup();
    const card = await screen.findByTestId('mcp-proposal');
    expect(card).toHaveTextContent('This repository proposes MCP servers (.mcp.json)');
    expect(screen.getByTestId('mcp-proposed-fs')).toHaveTextContent('npx -y @modelcontextprotocol/server-filesystem .');
    expect(screen.getByTestId('mcp-proposed-fs')).toHaveTextContent('API_TOKEN=se… (16 characters)');
    expect(screen.getByRole('checkbox', { name: 'local' })).toBeDisabled();
    expect(screen.getByTestId('mcp-proposed-local')).toHaveTextContent('Cannot be added');
    const add = screen.getByRole('button', { name: 'Add the selected servers' });
    expect(add).toBeDisabled();
    await userEvent.click(screen.getByRole('checkbox', { name: 'fs' }));
    await userEvent.click(add);
    await waitFor(() => expect(ipc.answers).toEqual([['fs']]));
    await waitFor(() => expect(screen.queryByTestId('mcp-proposal')).not.toBeInTheDocument());
  });

  it('"Not now" answers with no server', async () => {
    const ipc = setup();
    await userEvent.click(await screen.findByRole('button', { name: 'Not now' }));
    await waitFor(() => expect(ipc.answers).toEqual([[]]));
  });

  it('shows nothing without a proposal', async () => {
    const ipc = setup({ ...LIST, proposals: [] });
    await waitFor(() => expect(ipc.list).toHaveBeenCalled());
    expect(screen.queryByTestId('mcp-proposal')).not.toBeInTheDocument();
  });
});
