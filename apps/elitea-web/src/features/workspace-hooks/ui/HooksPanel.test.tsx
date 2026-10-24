/**
 * The hooks settings panel (#1237): both files with each hook's last run,
 * on/off, the JSON editor checked before a save, and the policy's word.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { HooksIpc, HooksList } from '@/shared/desktop/hooksIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { HooksPanel } from './HooksPanel';
import { HooksProposals } from './HooksProposals';

const LIST: HooksList = {
  allowed: false,
  user: { path: '/config/hooks.json', hooks: [{ id: 'fmt', event: 'PostToolUse', command: ['cargo', 'fmt', '--', '{path}'], sandbox: 'workspace-write' }] },
  workspace: { path: '/data/workspaces/w1/hooks.json', hooks: [] },
  last_runs: [{ id: 'fmt', event: 'PostToolUse', result: 'timed_out', duration_ms: 10000, at_ms: 1, message: 'it ran longer than 10000 ms and was stopped' }],
  proposals: [
    {
      file: '.elitea/hooks.json',
      file_sha256: 'abc',
      hooks: [
        { id: 'guard', event: 'PreToolUse', command: ['/usr/bin/guard'] },
        { id: 'bad', event: 'PreToolUse', command: ['./guard.sh'], problem: 'the program is a bare name or an absolute path' },
      ],
    },
  ],
};

function setup() {
  const mocks = {
    list: vi.fn(() => Promise.resolve(LIST)),
    save: vi.fn(() => Promise.resolve()),
    setEnabled: vi.fn(() => Promise.resolve(true)),
    answerProposal: vi.fn(() => Promise.resolve({ accepted: ['guard'] })),
    onProposal: vi.fn(() => Promise.resolve(() => undefined)),
  };
  const ipc: HooksIpc = mocks;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <HooksPanel ipc={ipc} workspaceId="w1" />
      <HooksProposals ipc={ipc} workspaceId="w1" />
    </QueryClientProvider>,
  );
  return mocks;
}

describe('HooksPanel', { timeout: 20_000 }, () => {
  it('lists the hooks with their last run and toggles one', async () => {
    const ipc = setup();
    const row = await screen.findByTestId('hook-user-fmt');
    expect(row).toHaveTextContent('cargo fmt -- {path}');
    expect(screen.getByTestId('hook-run-fmt')).toHaveTextContent('Timed out after 10000 ms');
    expect(screen.getByTestId('hooks-not-allowed')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('switch', { name: 'Run fmt' }));
    await waitFor(() => expect(ipc.setEnabled).toHaveBeenCalledWith('user', 'fmt', false, 'w1'));
  });

  it('checks the edited JSON before it saves', async () => {
    const ipc = setup();
    await screen.findByTestId('hook-user-fmt');
    await userEvent.click(screen.getAllByRole('button', { name: 'Edit as JSON' })[1]!);
    const editor = screen.getByLabelText('Hooks (JSON)');
    fireEvent.change(editor, { target: { value: '[{"id":"x","event":"Nope","command":["a"]}]' } });
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(ipc.save).not.toHaveBeenCalled();
    expect(screen.getByText(/event is PreToolUse/)).toBeInTheDocument();
    fireEvent.change(editor, { target: { value: '[{"id":"x","event":"Stop","command":["/bin/echo"]}]' } });
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(ipc.save).toHaveBeenCalledWith('user', [{ id: 'x', event: 'Stop', command: ['/bin/echo'] }], 'w1'));
  });

  it('trusts only the hooks ticked on a proposal', async () => {
    const ipc = setup();
    const card = await screen.findByTestId('hooks-proposal');
    expect(card).toHaveTextContent('This repository proposes 2 hooks (.elitea/hooks.json)');
    expect(screen.getByTestId('hook-proposed-bad')).toHaveTextContent('Cannot be trusted');
    await userEvent.click(screen.getByRole('checkbox', { name: 'guard' }));
    await userEvent.click(screen.getByRole('button', { name: 'Trust the selected hooks' }));
    await waitFor(() => expect(ipc.answerProposal).toHaveBeenCalledWith('w1', 'abc', ['guard']));
  });
});
