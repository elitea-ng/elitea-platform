/**
 * Pinning a project skill to a local thread from the composer's "/" menu (#2183, ADR-0050 amendment 1, A1.5): the row
 * of a project skill offers "Pin to this thread", the host keeps the pin (the fake), a pinned row says so and offers
 * "Unpin", and a refusal is said above the composer. A skill of the agent version is chosen by name, never pinned.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { createFakeWorkspaceIpc, type FakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { useDesktopLayout } from '@/widgets/desktop-shell';

import type { ComposerSkill } from './useAgentSkills';
import { useThreadSkillPins } from './useThreadSkillPins';
import { WorkspaceComposer } from './WorkspaceComposer';

const SKILLS: ComposerSkill[] = [
  { id: '3', name: 'Style' },
  { id: '12', name: 'Deploy', project: true },
];

function Composer({ ipc, conversationId, local }: { ipc: FakeWorkspaceIpc; conversationId: string; local: boolean }): React.JSX.Element {
  const skillPins = useThreadSkillPins(ipc, 'w1', 7, conversationId, local);
  return (
    <WorkspaceComposer
      ipc={ipc}
      workspaceId="w1"
      conversationId={conversationId}
      canSend
      busy={false}
      planMode={false}
      onPlanModeChange={() => undefined}
      skills={SKILLS}
      skillPins={skillPins}
      customCommands={[]}
      onSend={vi.fn().mockResolvedValue(true)}
      onStop={() => undefined}
      onCommand={() => undefined}
    />
  );
}

async function mount(ipc: FakeWorkspaceIpc, conversationId = '42', local = true): Promise<void> {
  useDesktopLayout.getState().setSession({ workspaceId: 'w1', newThread: () => undefined, togglePlanMode: () => undefined });
  const rootRoute = createRootRoute({ component: () => <Composer ipc={ipc} conversationId={conversationId} local={local} /> });
  const router = createRouter({ routeTree: rootRoute.addChildren([createRoute({ getParentRoute: () => rootRoute, path: '/x', component: () => null })]), history: createMemoryHistory({ initialEntries: ['/'] }) });
  render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  await screen.findByRole('textbox');
}

afterEach(() => useDesktopLayout.getState().setSession(null));

describe('the "/" menu on a local thread', () => {
  it('pins a project skill through the host, shows it pinned, and unpins it', async () => {
    const ipc = createFakeWorkspaceIpc();
    await mount(ipc);
    const user = userEvent.setup();
    await user.type(screen.getByRole('textbox'), '/');
    // Only the project skill can be pinned: the agent's own skill is chosen by name.
    const pin = await screen.findByRole('button', { name: 'Pin to this thread' });
    expect(screen.getAllByRole('button', { name: 'Pin to this thread' })).toHaveLength(1);
    await user.click(pin);
    await waitFor(() => expect(ipc.calls.pins).toEqual([{ workspaceId: 'w1', conversationId: '42', skillId: 12, pinned: true }]));
    expect(await screen.findByText('Pinned')).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: 'Unpin' }));
    await waitFor(() => expect(ipc.calls.pins[1]).toEqual({ workspaceId: 'w1', conversationId: '42', skillId: 12, pinned: false }));
    expect(await screen.findByRole('button', { name: 'Pin to this thread' })).toBeInTheDocument();
    expect(await ipc.threadSkills('w1', '42')).toEqual([]);
  });

  it('shows a pin the host already holds, read when the thread opens', async () => {
    const ipc = createFakeWorkspaceIpc();
    await ipc.pinThreadSkill('w1', '42', { projectId: 7, skillId: 12 });
    await mount(ipc);
    await userEvent.setup().type(screen.getByRole('textbox'), '/');
    expect(await screen.findByText('Pinned')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Unpin' })).toBeInTheDocument();
  });

  it('says why the host refused a pin', async () => {
    const ipc = createFakeWorkspaceIpc();
    ipc.failNext('pinThreadSkill', 'thread_skills_full', 'A thread holds at most five skills. Remove one to add another.');
    await mount(ipc);
    const user = userEvent.setup();
    await user.type(screen.getByRole('textbox'), '/');
    await user.click(await screen.findByRole('button', { name: 'Pin to this thread' }));
    expect(await screen.findByTestId('skill-pin-error')).toHaveTextContent('A thread holds at most five skills');
  });

  it('offers no pin on a thread without a saved conversation', async () => {
    const fresh = createFakeWorkspaceIpc();
    await mount(fresh, '');
    await userEvent.setup().type(screen.getByRole('textbox'), '/');
    await screen.findByRole('option', { name: '/Deploy' });
    expect(screen.queryByRole('button', { name: 'Pin to this thread' })).toBeNull();
  });

  it('lists a pin whose skill is gone so it can be unpinned', async () => {
    const ipc = createFakeWorkspaceIpc();
    await ipc.pinThreadSkill('w1', '42', { projectId: 7, skillId: 99 });
    ipc.setPinAvailable('w1', '42', 99, false);
    await mount(ipc);
    const user = userEvent.setup();
    await user.type(screen.getByRole('textbox'), '/');
    expect(await screen.findByText('Skill 99 is not available')).toBeInTheDocument();
    // The one Unpin is the stale row's.
    await user.click(await screen.findByRole('button', { name: 'Unpin' }));
    await waitFor(() => expect(ipc.calls.pins.at(-1)).toEqual({ workspaceId: 'w1', conversationId: '42', skillId: 99, pinned: false }));
    await waitFor(() => expect(screen.queryByText('Skill 99 is not available')).toBeNull());
  });

  it('says so when the thread\u2019s pins cannot be read', async () => {
    const ipc = createFakeWorkspaceIpc();
    ipc.failNext('threadSkills', 'storage', 'The thread history could not be read.');
    await mount(ipc);
    expect(await screen.findByTestId('skill-pin-error')).toHaveTextContent('could not be read');
  });
});
