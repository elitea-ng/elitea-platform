/**
 * Addressing an agent in a Local work thread (ADR-0061 decision 2b, issue 3056): the "@" menu lists the thread's
 * agents before the folder's files, an agent pick inserts the text Main routes on, a file pick is unchanged; the To
 * chip names who answers by Main's rule; the Participants tab's Address puts `@Name ` into the composer.
 */
import { act, fireEvent, render, renderHook, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { createFakeWorkspaceIpc, type FakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { useDesktopLayout } from '@/widgets/desktop-shell';

import { matchingAgents, requestAddress, useAddressRequests } from './useComposerMentions';
import type { ThreadAgent } from './useEnsureConversation';
import { WorkspaceComposer } from './WorkspaceComposer';
import { toTarget, WorkspaceToChip } from './WorkspaceToChip';

const MIRA: ThreadAgent = { participantId: 11, applicationId: 5, name: 'Mira', versionId: 0, cloud: false };
const DOCTOR: ThreadAgent = { participantId: 12, applicationId: 6, name: 'CI Doctor', versionId: 0, cloud: true };
const AGENTS = [MIRA, DOCTOR];
const FILES = [{ path: 'src/mirror.rs', kind: 'file' as const }, { path: 'src/main.rs', kind: 'file' as const }];
const LEAD_META = { floor: { policy: 'lead', lead_participant_id: 12 } };

async function mount(ipc: FakeWorkspaceIpc, onSend = vi.fn().mockResolvedValue(true), conversationId = '42', agents: readonly ThreadAgent[] = AGENTS): Promise<HTMLElement> {
  useDesktopLayout.getState().setSession({ workspaceId: 'w1', newThread: () => undefined, togglePlanMode: () => undefined });
  const Composer = () => (
    <WorkspaceComposer
      ipc={ipc}
      workspaceId="w1"
      conversationId={conversationId}
      canSend
      busy={false}
      planMode={false}
      onPlanModeChange={() => undefined}
      skills={[]}
      customCommands={[]}
      onSend={onSend}
      onStop={() => undefined}
      onCommand={() => undefined}
      agents={agents}
    />
  );
  const rootRoute = createRootRoute({ component: Composer });
  const router = createRouter({ routeTree: rootRoute.addChildren([createRoute({ getParentRoute: () => rootRoute, path: '/x', component: () => null })]), history: createMemoryHistory({ initialEntries: ['/'] }) });
  render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return screen.findByTestId('chat-message-input');
}

afterEach(() => useDesktopLayout.getState().setSession(null));

describe('the "@" menu of a team thread (issue 3056)', () => {
  it('lists the matching agent before the files, and picking it inserts its address', async () => {
    const ipc = createFakeWorkspaceIpc({ files: FILES });
    const input = await mount(ipc);
    const user = userEvent.setup();
    await user.type(input, '@Mi');
    const menu = await screen.findByTestId('workspace-file-menu');
    await waitFor(() => expect(within(menu).getAllByRole('option')).toHaveLength(2));
    const options = within(menu).getAllByRole('option');
    expect(options[0]).toHaveTextContent('@Mira');
    expect(options[0]).toHaveTextContent('Local');
    expect(options[1]).toHaveTextContent('src/mirror.rs');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(input).toHaveValue('@Mira '));
  });

  it('still adds a picked file to the mentions it sends', async () => {
    const ipc = createFakeWorkspaceIpc({ files: FILES });
    const onSend = vi.fn().mockResolvedValue(true);
    const input = await mount(ipc, onSend);
    const user = userEvent.setup();
    await user.type(input, '@main');
    const menu = await screen.findByTestId('workspace-file-menu');
    await waitFor(() => expect(within(menu).getAllByRole('option')).toHaveLength(1));
    await user.keyboard('{Enter}');
    await waitFor(() => expect(input).toHaveValue('@src/main.rs '));
    await user.keyboard('{Enter}');
    await waitFor(() => expect(onSend).toHaveBeenCalled());
    expect(onSend.mock.calls[0]?.[1]).toEqual(['src/main.rs']);
  });

  it('offers no agents with one agent in the thread', () => {
    expect(matchingAgents('', [MIRA])).toEqual([MIRA]);
    expect(matchingAgents('doc', AGENTS)).toEqual([DOCTOR]);
    expect(matchingAgents('zz', AGENTS)).toEqual([]);
  });
});

describe('the To chip (issue 3056)', () => {
  it('names the lead, then the agent the text addresses, and is absent with one agent', () => {
    expect(toTarget('fix it', AGENTS, LEAD_META, 0)).toEqual({ agent: DOCTOR, isLead: true });
    expect(toTarget('@Mira fix it', AGENTS, LEAD_META, 0)).toEqual({ agent: MIRA, isLead: false });
    expect(toTarget('fix it', AGENTS, {}, 11)).toEqual({ agent: MIRA, isLead: false });
    expect(toTarget('fix it', [MIRA], LEAD_META, 11)).toBeUndefined();

    const onAddress = vi.fn();
    const { rerender } = renderWithTheme(<WorkspaceToChip draft="" agents={AGENTS} meta={LEAD_META} fallbackParticipantId={0} onAddress={onAddress} />);
    expect(screen.getByTestId('workspace-to-chip')).toHaveTextContent('To: CI Doctor (lead)');
    rerender(<WorkspaceToChip draft="@CI Doctor look" agents={AGENTS} meta={{}} fallbackParticipantId={11} onAddress={onAddress} />);
    expect(screen.getByTestId('workspace-to-chip')).toHaveTextContent('To: CI Doctor');
    fireEvent.click(screen.getByTestId('workspace-to-chip'));
    fireEvent.click(screen.getAllByTestId('workspace-to-chip-option')[0]!);
    expect(onAddress).toHaveBeenCalledWith(MIRA);
    rerender(<WorkspaceToChip draft="" agents={[MIRA]} meta={LEAD_META} fallbackParticipantId={0} onAddress={onAddress} />);
    expect(screen.queryByTestId('workspace-to-chip')).toBeNull();
  });
});

describe('Address from the Participants tab (issue 3056)', () => {
  it('appends the address to the composer of its thread, and does not replay an earlier one', async () => {
    requestAddress('42', 'Docs Writer');
    const input = await mount(createFakeWorkspaceIpc());
    expect(input).toHaveValue('');
    act(() => requestAddress('99', 'Mira'));
    expect(input).toHaveValue('');
    await userEvent.setup().type(input, 'look');
    act(() => requestAddress('42', 'CI Doctor'));
    await waitFor(() => expect(input).toHaveValue('look @CI Doctor '));
  });

  it('keeps its count per thread: a composer that moves to another thread takes that thread\'s next address', () => {
    let text = '';
    const handle = { getInputContent: () => text, replaceRange: (start: number, end: number, value: string) => { text = text.slice(0, start) + value + text.slice(end); } };
    const ref = { current: handle };
    const { rerender } = renderHook(({ thread }: { thread: string }) => useAddressRequests(thread, ref), { initialProps: { thread: 'a1' } });
    act(() => { requestAddress('a1', 'Mira'); requestAddress('a1', 'Mira'); requestAddress('a1', 'Mira'); });
    expect(text).toBe('@Mira ');
    text = '';
    rerender({ thread: 'b1' });
    act(() => requestAddress('b1', 'CI Doctor'));
    expect(text).toBe('@CI Doctor ');
  });
});
