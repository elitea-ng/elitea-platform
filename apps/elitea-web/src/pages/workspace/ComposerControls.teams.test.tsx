/**
 * A Local work thread's agents are managed in its Participants tab, not in the composer's chip (#3081): the chip says
 * who the message goes to and opens the tab; the tab's panel lists the agents that join with the next message, with
 * Make lead and Remove, and the Add agent, Add cloud agent and Add team search pickers (a team that cannot run here is
 * disabled with its reason).
 */
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { Application } from '@/shared/api/generated/model';
import { SidePanelSlotProvider, type SidePanelSlot } from '@/shared/desktop/inspectorSlot';
import { renderWithTheme as render } from '@/shared/ui/lib/testTheme';

import { AgentChip, agentChipLabel } from './ComposerControls';
import { filterOptions } from './SearchPicker';
import { ThreadAgentsPanel } from './ThreadAgentsPanel';
import type { AgentSelection } from './useAgentSelection';
import type { WorkTeam } from './workTeams';

const crew: WorkTeam = {
  id: 3,
  name: 'Review crew',
  lead: true,
  primary: { applicationId: 6, versionId: 10, name: 'Reviewer' },
  others: [{ applicationId: 5, versionId: 9, name: 'Coder' }],
};

function selection(extra: Partial<AgentSelection>): AgentSelection {
  return {
    agents: [{ id: '5', name: 'Coder' }, { id: '6', name: 'Reviewer' }, { id: '7', name: 'Docs Writer' }] as unknown as Application[],
    versions: [{ id: 9, name: 'base', agentType: 'openai' }],
    conversations: [],
    agentId: '5',
    versionId: '9',
    conversationId: '',
    loading: false,
    empty: false,
    selectAgent: vi.fn(),
    selectVersion: vi.fn(),
    selectConversation: vi.fn(),
    ...extra,
  };
}

const reviewer = { agentId: '6', name: 'Reviewer', versionId: 10, agentType: 'openai', cloud: true, versionName: 'v2' };

describe('the agent chip (#3081)', () => {
  it('names the agent and its version with one agent, and who a message goes to with several', () => {
    expect(agentChipLabel(selection({}))).toBe('Coder · base');
    const two = selection({ companions: [reviewer] });
    expect(agentChipLabel(two, 'fix it')).toBe('To: Coder');
    expect(agentChipLabel(two, '@Reviewer check it')).toBe('To: Reviewer');
    const team = selection({ agentId: '6', versionId: '10', companions: [{ agentId: '5', name: 'Coder', versionId: 9, agentType: '', cloud: false }], team: crew });
    expect(agentChipLabel(team, 'go')).toBe('Review crew · To: Reviewer (lead)');
  });

  it('follows the thread’s lead once the thread exists', () => {
    const floor = {
      agents: [
        { participantId: 11, applicationId: 5, name: 'Coder', versionId: 9, cloud: false },
        { participantId: 12, applicationId: 6, name: 'Reviewer', versionId: 10, cloud: true },
      ],
      meta: { floor: { policy: 'lead', lead_participant_id: 12 } },
    };
    const existing = selection({ conversationId: '77' });
    expect(agentChipLabel(existing, 'fix it', floor)).toBe('To: Reviewer (lead)');
    expect(agentChipLabel(existing, '@Coder fix it', floor)).toBe('To: Coder');
  });

  it('opens the Participants tab, with no menu of its own', async () => {
    const user = userEvent.setup();
    const show = vi.fn();
    const slot: SidePanelSlot = { register: () => () => undefined, targets: new Map(), show };
    render(
      <SidePanelSlotProvider value={slot}>
        <AgentChip selection={selection({ companions: [reviewer] })} />
      </SidePanelSlotProvider>,
    );
    await user.click(screen.getByTestId('agent-chip'));
    expect(show).toHaveBeenCalledWith('team');
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('is the To picker in a thread of several agents: addresses one, or opens the tab (#3145)', async () => {
    const user = userEvent.setup();
    const show = vi.fn();
    const onAddress = vi.fn();
    const slot: SidePanelSlot = { register: () => () => undefined, targets: new Map(), show };
    const floor = {
      agents: [
        { participantId: 11, applicationId: 5, name: 'Coder', versionId: 9, cloud: false },
        { participantId: 12, applicationId: 6, name: 'Reviewer', versionId: 10, cloud: true },
      ],
      meta: { floor: { policy: 'lead', lead_participant_id: 12 } },
    };
    render(
      <SidePanelSlotProvider value={slot}>
        <AgentChip selection={selection({ conversationId: '77' })} floor={floor} onAddress={onAddress} />
      </SidePanelSlotProvider>,
    );
    const chip = screen.getByTestId('agent-chip');
    expect(chip).toHaveTextContent('To: Reviewer (lead)');
    expect(chip).toHaveAttribute('aria-haspopup', 'menu');
    await user.click(chip);
    const options = await screen.findAllByTestId('workspace-to-chip-option');
    expect(options.map((option) => option.textContent)).toEqual(['Coder', 'Reviewer']);
    await user.click(options[0]!);
    expect(onAddress).toHaveBeenCalledWith(floor.agents[0]);
    await user.click(chip);
    await user.click(await screen.findByTestId('agent-chip-manage'));
    expect(show).toHaveBeenCalledWith('team');
  });
});

describe('the Participants tab’s agents panel (#3081)', () => {
  it('lists a new thread’s agents with the lead, its version, and Make lead and Remove for the others', async () => {
    const user = userEvent.setup();
    const makeLead = vi.fn();
    const removeCompanion = vi.fn();
    const writer = { agentId: '7', name: 'Docs Writer', versionId: 11, agentType: 'openai', cloud: false, versionName: 'base' };
    render(<ThreadAgentsPanel selection={selection({ companions: [reviewer, writer], makeLead, removeCompanion, addCompanion: vi.fn(), team: crew })} joined={[]} locked={false} />);
    const rows = screen.getAllByTestId('thread-pending-agent');
    expect(rows.map((row) => row.getAttribute('data-agent-id'))).toEqual(['5', '6', '7']);
    expect(within(rows[0] as HTMLElement).getByTestId('thread-pending-lead')).toHaveTextContent('Lead');
    expect(within(rows[0] as HTMLElement).getByTestId('thread-lead-version')).toHaveTextContent('base');
    expect(rows[1]).toHaveTextContent('Cloud');
    expect(rows[1]).toHaveTextContent('v2');
    expect(screen.getByTestId('thread-team-name')).toHaveTextContent('Team: Review crew');
    // A cloud agent is not made lead: the composer's agent runs on this computer.
    expect(within(rows[1] as HTMLElement).queryByTestId('thread-pending-make-lead')).toBeNull();
    await user.click(within(rows[2] as HTMLElement).getByTestId('thread-pending-make-lead'));
    expect(makeLead).toHaveBeenCalledWith('7');
    await user.click(within(rows[1] as HTMLElement).getByTestId('thread-pending-remove'));
    expect(removeCompanion).toHaveBeenCalledWith('6');
  });

  it('adds an agent through a search picker, and offers only agents not yet added that way', async () => {
    const user = userEvent.setup();
    const addCompanion = vi.fn();
    render(<ThreadAgentsPanel selection={selection({ companions: [reviewer], addCompanion })} joined={[]} locked={false} />);
    await user.click(screen.getByTestId('thread-add-cloud-agent'));
    const cloud = await screen.findByTestId('thread-add-cloud-agent-picker');
    // Reviewer is already a cloud agent of the thread; Coder is the lead.
    expect(within(cloud).queryByTestId('thread-add-cloud-agent-option-6')).toBeNull();
    expect(within(cloud).queryByTestId('thread-add-cloud-agent-option-5')).toBeNull();
    await user.keyboard('{Escape}');
    await user.click(screen.getByTestId('thread-add-agent'));
    const picker = await screen.findByTestId('thread-add-agent-picker');
    await user.type(within(picker).getByRole('textbox'), 'docs');
    expect(within(picker).queryByTestId('thread-add-agent-option-6')).toBeNull();
    await user.click(within(picker).getByTestId('thread-add-agent-option-7'));
    expect(addCompanion).toHaveBeenCalledWith('7', false);
  });

  it('adds a team: a team that cannot run here is disabled and says why', async () => {
    const user = userEvent.setup();
    const selectTeam = vi.fn();
    render(
      <ThreadAgentsPanel
        selection={selection({
          addCompanion: vi.fn(),
          teams: [
            { id: 3, name: 'Review crew', team: crew },
            { id: 4, name: 'Mixed crew', unavailable: 'other_project' },
            { id: 5, name: 'Build crew', unavailable: 'pipeline' },
          ],
          selectTeam,
        })}
        joined={[]}
        locked={false}
      />,
    );
    await user.click(screen.getByTestId('thread-add-team'));
    const mixed = await screen.findByTestId('thread-add-team-option-4');
    expect(mixed).toHaveAttribute('aria-disabled', 'true');
    expect(mixed).toHaveTextContent('Agents of another project');
    expect(screen.getByTestId('thread-add-team-option-5')).toHaveTextContent('Has a pipeline');
    await user.click(screen.getByTestId('thread-add-team-option-3'));
    expect(selectTeam).toHaveBeenCalledWith(3);
  });

  it('offers no Add team when the project has no team (or the caller may not list them), and nothing while the thread is in the cloud', () => {
    const { unmount } = render(<ThreadAgentsPanel selection={selection({ addCompanion: vi.fn(), teams: [], selectTeam: vi.fn() })} joined={[]} locked={false} />);
    expect(screen.queryByTestId('thread-add-team')).toBeNull();
    unmount();
    render(<ThreadAgentsPanel selection={selection({ addCompanion: vi.fn() })} joined={[]} locked />);
    expect(screen.getByTestId('thread-add-agent')).toBeDisabled();
  });

  it('after the first send lists only the agents added since', () => {
    const joined = [{ participantId: 11, applicationId: 5, name: 'Coder', versionId: 9, cloud: false }];
    render(<ThreadAgentsPanel selection={selection({ conversationId: '77', companions: [reviewer], addCompanion: vi.fn() })} joined={joined} locked={false} />);
    const rows = screen.getAllByTestId('thread-pending-agent');
    expect(rows.map((row) => row.getAttribute('data-agent-id'))).toEqual(['6']);
    expect(screen.getByText('Join with your next message')).toBeInTheDocument();
    expect(screen.queryByTestId('thread-pending-make-lead')).toBeNull();
  });

  it('filters by any part of the name, in any case', () => {
    const options = [{ id: '1', label: 'Docs Writer' }, { id: '2', label: 'Coder' }];
    expect(filterOptions(options, 'WRIT').map((o) => o.id)).toEqual(['1']);
    expect(filterOptions(options, ' ')).toEqual(options);
  });
});
