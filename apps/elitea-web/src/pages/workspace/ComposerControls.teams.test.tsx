/**
 * The workspace agent picker's "Run as a team ›" (#3053): the project's saved teams, a team that cannot run here
 * disabled with its reason, and the chip naming the team that runs the thread.
 */
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { Application } from '@/shared/api/generated/model';
import { renderWithTheme as render } from '@/shared/ui/lib/testTheme';

import { AgentChip, agentChipLabel } from './ComposerControls';
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
    agents: [{ id: '5', name: 'Coder' }, { id: '6', name: 'Reviewer' }] as unknown as Application[],
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

describe('Run as a team (#3053)', () => {
  it('lists the teams, disables one that cannot run here with its reason, and picks a runnable one', async () => {
    const user = userEvent.setup();
    const selectTeam = vi.fn();
    render(
      <AgentChip
        selection={selection({
          teams: [
            { id: 3, name: 'Review crew', team: crew },
            { id: 4, name: 'Mixed crew', unavailable: 'other_project' },
            { id: 5, name: 'Build crew', unavailable: 'pipeline' },
          ],
          selectTeam,
        })}
      />,
    );
    await user.click(screen.getByTestId('agent-chip'));
    await user.click(await screen.findByTestId('agent-team'));
    const mixed = await screen.findByTestId('agent-team-4');
    expect(mixed).toHaveAttribute('aria-disabled', 'true');
    expect(mixed).toHaveTextContent('Agents of another project');
    expect(screen.getByTestId('agent-team-5')).toHaveTextContent('Has a pipeline');
    await user.click(screen.getByTestId('agent-team-3'));
    expect(selectTeam).toHaveBeenCalledWith(3);
  });

  it('offers no team row when the project has none (or the caller may not list them)', async () => {
    const user = userEvent.setup();
    render(<AgentChip selection={selection({ teams: [], selectTeam: vi.fn() })} />);
    await user.click(screen.getByTestId('agent-chip'));
    expect(await screen.findByText('Version')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-team')).toBeNull();
  });

  it('names the team that runs the thread on the chip, before its agents', () => {
    const picked = selection({
      agentId: '6',
      versionId: '10',
      versions: [{ id: 10, name: 'base', agentType: 'openai' }],
      companions: [{ agentId: '5', name: 'Coder', versionId: 9, agentType: '', cloud: false }],
      team: crew,
    });
    expect(agentChipLabel(picked)).toBe('Review crew: Reviewer · base + Coder');
    expect(agentChipLabel(selection({}))).toBe('Coder · base');
  });
});
