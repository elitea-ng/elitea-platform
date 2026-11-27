/**
 * Issue 3142: the Edit team dialog lists the members as a roster with their
 * avatars and the Lead badge, and every row action is named for the agent it
 * acts on, explained by a tooltip, and reachable by keyboard.
 */
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { AgentTeam } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { renderWithQueryAndTheme } from '@/test/renderWithQueryAndTheme';

import type { MemberAvatar } from '../model/useTeamMemberAvatars';
import { TeamEditDialog } from './TeamEditDialog';

const BASE = '/api/v2';
const APPLICATIONS = `${BASE}/elitea_core/applications/prompt_lib/:projectId`;
const TEAM = `${BASE}/elitea_core/agent_team/prompt_lib/:projectId/:teamId`;

const team: AgentTeam = {
  id: 3,
  project_id: 1,
  name: 'Release crew',
  description: 'Writes the notes',
  floor_policy: 'lead',
  lead_position: 1,
  members: [
    { application_id: 31, project_id: 1, version_id: 41, name: 'Mira' },
    { application_id: 32, project_id: 1, version_id: 42, name: 'CI Doctor' },
  ],
  created_by: 7,
  created_at: '2026-11-09T10:00:00Z',
  updated_at: '2026-11-09T10:00:00Z',
};

const renderAvatar = vi.fn((avatar: MemberAvatar, size: string) => (
  <img
    alt=""
    data-testid="live-avatar"
    data-size={size}
    src={avatar.iconUrl}
  />
));

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  renderAvatar.mockClear();
  server.use(
    http.get(APPLICATIONS, () =>
      HttpResponse.json({
        rows: [
          { id: '31', project_id: '1', name: 'Mira', meta: { icon_meta: { url: '/icons/mira.png' } } },
          { id: '32', project_id: '1', name: 'CI Doctor' },
          { id: '33', project_id: '1', name: 'Scribe', meta: { icon_meta: { url: '/icons/scribe.png' } } },
        ],
        total: 3,
      }),
    ),
  );
});
afterEach(() => resetGeneratedClient());

function renderDialog(): void {
  renderWithQueryAndTheme(
    <TeamEditDialog
      projectId="1"
      open
      team={team}
      renderAvatar={renderAvatar}
      onClose={vi.fn()}
    />,
  );
}

describe('TeamEditDialog (issue 3142)', () => {
  it('draws each member with its avatar, name and the Lead badge', async () => {
    renderDialog();
    const roster = screen.getByRole('list', { name: 'Agents of this team' });
    const rows = within(roster).getAllByRole('listitem');
    expect(rows).toHaveLength(2);
    // Mira has an icon: the shared renderer draws it; CI Doctor has none: the monogram.
    await waitFor(() => expect(within(rows[0] as HTMLElement).getByTestId('live-avatar')).toHaveAttribute('src', '/icons/mira.png'));
    expect(within(rows[1] as HTMLElement).queryByTestId('live-avatar')).toBeNull();
    expect(within(rows[1] as HTMLElement).getByText('CD')).toBeInTheDocument();
    expect(within(rows[0] as HTMLElement).getByTestId('agent-roster-lead-badge')).toHaveTextContent('Lead');
    expect(within(rows[1] as HTMLElement).queryByTestId('agent-roster-lead-badge')).toBeNull();
    expect(screen.getByTestId('agent-team-edit-count')).toHaveTextContent('2 of 16');
  });

  it('names every row action for its agent, says the lead state, and reorders with the lead kept', async () => {
    const user = userEvent.setup();
    let saved: unknown;
    server.use(
      http.put(TEAM, async ({ request }) => {
        saved = await request.json();
        return HttpResponse.json({ ...team });
      }),
    );
    renderDialog();
    const lead = screen.getByRole('button', { name: 'Clear Mira as the lead' });
    expect(lead).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: 'Make CI Doctor the lead' })).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getByRole('button', { name: 'Remove CI Doctor from the team' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Move Mira up' })).toHaveAttribute('aria-disabled', 'true');
    // The tooltip says the same words on hover.
    await user.hover(screen.getByRole('button', { name: 'Remove Mira from the team' }));
    expect(await screen.findByRole('tooltip')).toHaveTextContent('Remove Mira from the team');

    // Keyboard: focus Move Mira down and press Enter.
    screen.getByRole('button', { name: 'Move Mira down' }).focus();
    await user.keyboard('{Enter}');
    const rows = within(screen.getByRole('list', { name: 'Agents of this team' })).getAllByRole('listitem');
    expect(rows.map((row) => row.textContent)).toEqual([expect.stringContaining('CI Doctor'), expect.stringContaining('Mira')]);
    expect(within(rows[1] as HTMLElement).getByTestId('agent-roster-lead-badge')).toBeInTheDocument();
    // Mira is now last: its Move down turned inert but kept the focus.
    expect(screen.getByRole('button', { name: 'Move Mira down' })).toHaveFocus();
    expect(screen.getByRole('button', { name: 'Move Mira down' })).toHaveAttribute('aria-disabled', 'true');
    await user.keyboard('{Enter}');
    expect(within(screen.getByRole('list', { name: 'Agents of this team' })).getAllByRole('listitem')[1]).toHaveTextContent('Mira');

    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(saved).toBeDefined());
    expect(saved).toMatchObject({ lead_position: 2, members: [{ application_id: 32 }, { application_id: 31 }] });
  });

  it('offers the project agents in the picker with their avatars', async () => {
    const user = userEvent.setup();
    renderDialog();
    const input = screen.getByTestId('agent-team-edit-add');
    await user.click(input);
    const option = await screen.findByTestId('agent-team-edit-add-33');
    expect(within(option).getByTestId('live-avatar')).toHaveAttribute('src', '/icons/scribe.png');
    expect(option).toHaveTextContent('Scribe');
  });
});
