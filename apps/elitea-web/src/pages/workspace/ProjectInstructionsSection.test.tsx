/**
 * The project instructions in a Local work thread's Participants tab (issue 3130): the folder's AGENTS.md listed
 * once, the thread's "Respect AGENTS.md" switch (on by default, kept by the host, so a reopened thread shows its
 * choice), the plain "ignored" line when it is off, and no switch when the folder holds no instructions.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { replayView } from '@/features/workspace';
import { createFakeWorkspaceIpc, type FakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ProjectInstructionsSection } from './ProjectInstructionsSection';

const IDLE = { turnId: null, view: replayView([]) };

function mount(ipc: FakeWorkspaceIpc, conversationId = '77', turn: Parameters<typeof ProjectInstructionsSection>[0]['turn'] = IDLE) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithTheme(
    <QueryClientProvider client={client}>
      <ProjectInstructionsSection workspaceId="ws-1" conversationId={conversationId} turn={turn} ipc={ipc} />
    </QueryClientProvider>,
  );
}

const respectSwitch = () => screen.findByRole('switch', { name: 'Respect AGENTS.md' });

describe('the project instructions of a Local work thread (issue 3130)', () => {
  it('lists the root files and the nested ones the turn applied, with the switch on by default', async () => {
    const ipc = createFakeWorkspaceIpc();
    ipc.setInstructionFiles('ws-1', ['AGENTS.md', 'SOUL.md']);
    const turn = {
      turnId: 'turn-1',
      view: replayView([{ turn_id: 'turn-1', seq: 1, kind: 'status', payload: { phase: 'running', project_instructions: ['AGENTS.md', 'apps/web/AGENTS.md'], project_instructions_applied: true } }]),
    };
    mount(ipc, '77', turn);
    expect(await respectSwitch()).toBeChecked();
    await waitFor(() => expect(screen.getAllByTestId('project-instructions-file').map((node) => node.textContent)).toEqual(['AGENTS.md', 'SOUL.md', 'apps/web/AGENTS.md']));
    expect(screen.getByText('Project instructions')).toBeInTheDocument();
    expect(screen.queryByTestId('agents-md-ignored')).toBeNull();
  });

  it('turns it off for the thread through the host and says AGENTS.md is ignored, then on again', async () => {
    const ipc = createFakeWorkspaceIpc();
    ipc.setInstructionFiles('ws-1', ['AGENTS.md']);
    mount(ipc);
    const user = userEvent.setup();
    await user.click(await respectSwitch());
    await waitFor(() => expect(ipc.calls.respects).toEqual([{ workspaceId: 'ws-1', conversationId: '77', respect: false }]));
    expect(await screen.findByTestId('agents-md-ignored')).toHaveTextContent('AGENTS.md ignored in this thread');
    await waitFor(async () => expect(await respectSwitch()).not.toBeChecked());
    await user.click(await respectSwitch());
    await waitFor(() => expect(ipc.calls.respects.at(-1)).toEqual({ workspaceId: 'ws-1', conversationId: '77', respect: true }));
    await waitFor(() => expect(screen.queryByTestId('agents-md-ignored')).toBeNull());
  });

  it('shows the choice the host kept when the thread is opened again', async () => {
    const ipc = createFakeWorkspaceIpc();
    ipc.setInstructionFiles('ws-1', ['AGENTS.md']);
    await ipc.setThreadRespectAgentsMd('ws-1', '77', false);
    const first = mount(ipc);
    await waitFor(async () => expect(await respectSwitch()).not.toBeChecked());
    first.unmount();
    // Reopened (a new query cache, as after a restart): the host still answers off.
    mount(ipc);
    await waitFor(async () => expect(await respectSwitch()).not.toBeChecked());
    expect(screen.getByTestId('agents-md-ignored')).toBeInTheDocument();
    // Another thread of the folder keeps the default.
    mount(ipc, '78');
    await waitFor(() => expect(screen.getAllByRole('switch', { name: 'Respect AGENTS.md' })).toHaveLength(2));
    expect(screen.getAllByRole('switch', { name: 'Respect AGENTS.md' })[1]).toBeChecked();
  });

  it('shows no switch when the folder holds no AGENTS.md', async () => {
    const ipc = createFakeWorkspaceIpc();
    mount(ipc);
    expect(await screen.findByTestId('project-instructions-none')).toHaveTextContent('No AGENTS.md in this folder.');
    expect(screen.queryByRole('switch')).toBeNull();
  });

  it('keeps the switch on and disabled until the thread has started', async () => {
    const ipc = createFakeWorkspaceIpc();
    ipc.setInstructionFiles('ws-1', ['AGENTS.md']);
    mount(ipc, '');
    const control = await respectSwitch();
    expect(control).toBeChecked();
    expect(control).toBeDisabled();
    expect(screen.getByText('You can change this once the thread has started.')).toBeInTheDocument();
  });
});
