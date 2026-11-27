import { useState } from 'react';

import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { renderWithQueryAndTheme } from '@/test/renderWithQueryAndTheme';

import { SaveAsTeamDialog } from './SaveAsTeamDialog';
import { TeamsPanel, type TeamsPanelProps } from './TeamsPanel';

const BASE = '/api/v2';
const TEAMS = `${BASE}/elitea_core/agent_teams/prompt_lib/:projectId`;
const TEAM = `${BASE}/elitea_core/agent_team/prompt_lib/:projectId/:teamId`;
const START = `${BASE}/elitea_core/agent_team_conversation/prompt_lib/:projectId/:teamId`;
const FROM = `${BASE}/elitea_core/agent_team_from_conversation/prompt_lib/:projectId/:conversationId`;
const PERMISSIONS = `${BASE}/auth/permissions/prompt_lib/:projectId`;
const APPLICATIONS = `${BASE}/elitea_core/applications/prompt_lib/:projectId`;

/** The panel with its New team state held outside it, as the page holds it. */
function Panel(props: Partial<TeamsPanelProps> & Pick<TeamsPanelProps, 'onStarted'>): React.JSX.Element {
  const [creating, setCreating] = useState(false);
  return (
    <TeamsPanel
      projectId="1"
      creating={creating}
      onCreatingChange={setCreating}
      {...props}
    />
  );
}

const TEAM_ROW = {
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

function grant(...names: string[]): void {
  server.use(http.get(PERMISSIONS, () => HttpResponse.json(names.map((name) => ({ name, enabled: true })))));
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  // The project's agents, for the member avatars: Mira has an uploaded icon, CI Doctor none.
  server.use(
    http.get(APPLICATIONS, () =>
      HttpResponse.json({ rows: [{ id: '31', name: 'Mira', tags: [], created_at: '2026-11-01T00:00:00Z', updated_at: '2026-11-01T00:00:00Z', owner_id: '1', is_forked: false, has_interrupt: false, meta: { icon_meta: { url: '/media/mira.png' } } }], total: 1 }),
    ),
  );
});
afterEach(() => resetGeneratedClient());

describe('TeamsPanel', () => {
  it('lists the teams, marks the lead, and offers only what the caller may do', async () => {
    grant('models.teams.list');
    server.use(http.get(TEAMS, () => HttpResponse.json({ rows: [TEAM_ROW], total: 1 })));
    renderWithQueryAndTheme(<Panel onStarted={vi.fn()} />);
    expect(await screen.findByTestId('agent-team-name-3')).toHaveTextContent('Release crew');
    // The lead leads the stack, with its badge; the full list is one disclosure away.
    const faces = screen.getByTestId('agent-team-avatars-3');
    expect(faces.firstElementChild).toHaveAttribute('data-testid', 'agent-team-lead-avatar');
    expect(screen.getByTestId('agent-team-lead-badge')).toBeInTheDocument();
    const toggle = screen.getByTestId('agent-team-members-toggle-3');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByText('Mira (lead)')).toBeInTheDocument();
    expect(screen.getByText('CI Doctor')).toBeInTheDocument();
    expect(screen.getByTestId('agent-team-start-3')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-teams-create')).not.toBeInTheDocument();
    // Export stays in the overflow menu; Edit and Delete are not offered without their permissions.
    await userEvent.click(screen.getByTestId('agent-team-menu-3'));
    expect(await screen.findByTestId('agent-team-export-3')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-team-edit-3')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agent-team-delete-3')).not.toBeInTheDocument();
  });

  // #1847 / #1955: past one page the list says how many there are and loads the rest by keyset.
  it('pages past the first 200 teams by the last team held, not by offset', async () => {
    grant('models.teams.list');
    const cursors: string[] = [];
    const row = (id: number) => ({ ...TEAM_ROW, id, name: `Team ${id}` });
    server.use(
      http.get(TEAMS, ({ request }) => {
        const params = new URL(request.url).searchParams;
        expect(params.has('offset')).toBe(false);
        const after = params.get('after_id');
        cursors.push(after === null ? '-' : `${params.get('after_name')} / ${after}`);
        const rows = after === null ? Array.from({ length: 200 }, (_, i) => row(i + 1)) : [row(201)];
        return HttpResponse.json({ rows, total: 201 });
      }),
    );
    renderWithQueryAndTheme(<Panel onStarted={vi.fn()} />);
    expect(await screen.findByTestId('agent-teams-count')).toHaveTextContent('Showing 200 of 201 teams');
    expect(screen.queryByTestId('agent-team-name-201')).not.toBeInTheDocument();
    await userEvent.click(screen.getByTestId('agent-teams-more'));
    expect(await screen.findByTestId('agent-team-name-201')).toHaveTextContent('Team 201');
    expect(screen.queryByTestId('agent-teams-more')).not.toBeInTheDocument();
    expect(cursors).toEqual(['-', 'Team 200 / 200']);
  });

  it('stops at the total, not at a short page: a full last page ends the walk', async () => {
    grant('models.teams.list');
    const cursors: string[] = [];
    const row = (id: number) => ({ ...TEAM_ROW, id, name: `Team ${id}` });
    server.use(
      http.get(TEAMS, ({ request }) => {
        const after = new URL(request.url).searchParams.get('after_id');
        cursors.push(after ?? '-');
        return HttpResponse.json({ rows: Array.from({ length: 200 }, (_, i) => row(i + 1)), total: 200 });
      }),
    );
    renderWithQueryAndTheme(<Panel onStarted={vi.fn()} />);
    expect(await screen.findByTestId('agent-team-name-200')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-teams-more')).not.toBeInTheDocument();
    expect(cursors).toEqual(['-']);
  });

  it('shows a team the next page repeats once', async () => {
    grant('models.teams.list');
    const row = (id: number) => ({ ...TEAM_ROW, id, name: `Team ${id}` });
    server.use(
      http.get(TEAMS, ({ request }) => {
        const after = new URL(request.url).searchParams.get('after_id');
        // A page that repeats the last team held (a rename between pages).
        const rows = after === null ? Array.from({ length: 200 }, (_, i) => row(i + 1)) : [row(200), row(201)];
        return HttpResponse.json({ rows, total: 202 });
      }),
    );
    renderWithQueryAndTheme(<Panel onStarted={vi.fn()} />);
    await userEvent.click(await screen.findByTestId('agent-teams-more'));
    expect(await screen.findByTestId('agent-team-name-201')).toBeInTheDocument();
    expect(screen.getAllByTestId('agent-team-name-200')).toHaveLength(1);
  });

  it('offers no more when every team is shown', async () => {
    grant('models.teams.list');
    server.use(http.get(TEAMS, () => HttpResponse.json({ rows: [TEAM_ROW], total: 1 })));
    renderWithQueryAndTheme(<Panel onStarted={vi.fn()} />);
    await screen.findByTestId('agent-team-name-3');
    expect(screen.queryByTestId('agent-teams-more')).not.toBeInTheDocument();
  });

  it('starts a conversation and hands it to the page', async () => {
    grant('models.teams.list');
    let started = '';
    server.use(
      http.get(TEAMS, () => HttpResponse.json({ rows: [TEAM_ROW], total: 1 })),
      http.post(START, ({ params }) => {
        started = String(params['teamId']);
        return HttpResponse.json({ id: '77', project_id: '1', name: 'Release crew', created_at: '', updated_at: '', created_by: '7', message_count: 0 }, { status: 201 });
      }),
    );
    const onStarted = vi.fn();
    renderWithQueryAndTheme(<Panel onStarted={onStarted} />);
    await userEvent.click(await screen.findByTestId('agent-team-start-3'));
    await waitFor(() => expect(onStarted).toHaveBeenCalledWith(expect.objectContaining({ id: '77' })));
    expect(started).toBe('3');
  });

  it('shows a refused start in the server\'s words', async () => {
    grant('models.teams.list');
    server.use(
      http.get(TEAMS, () => HttpResponse.json({ rows: [TEAM_ROW], total: 1 })),
      http.post(START, () => HttpResponse.json({ error: 'agent 2 of the team does not exist or is not available to you', code: 'team_member_unavailable', position: 2 }, { status: 422 })),
    );
    renderWithQueryAndTheme(<Panel onStarted={vi.fn()} />);
    await userEvent.click(await screen.findByTestId('agent-team-start-3'));
    expect(await screen.findByTestId('agent-teams-start-error')).toHaveTextContent('agent 2 of the team');
  });

  it('deletes a team after confirming', async () => {
    grant('models.teams.list', 'models.teams.delete');
    let deleted = false;
    server.use(
      http.get(TEAMS, () => HttpResponse.json({ rows: deleted ? [] : [TEAM_ROW], total: deleted ? 0 : 1 })),
      http.delete(TEAM, () => {
        deleted = true;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    renderWithQueryAndTheme(<Panel onStarted={vi.fn()} />);
    await userEvent.click(await screen.findByTestId('agent-team-menu-3'));
    await userEvent.click(await screen.findByTestId('agent-team-delete-3'));
    // Nothing is deleted until the confirmation says so.
    expect(deleted).toBe(false);
    await userEvent.click(await screen.findByRole('button', { name: /delete|confirm/i }));
    expect(await screen.findByTestId('agent-teams-empty')).toBeInTheDocument();
  });

  it('explains what a team is and offers New team when there are none', async () => {
    grant('models.teams.list', 'models.teams.create');
    server.use(http.get(TEAMS, () => HttpResponse.json({ rows: [], total: 0 })));
    renderWithQueryAndTheme(<Panel onStarted={vi.fn()} />);
    const empty = await screen.findByTestId('agent-teams-empty');
    expect(empty).toHaveTextContent('No teams yet');
    expect(empty).toHaveTextContent('saved set of agents');
    await userEvent.click(within(empty).getByRole('button', { name: 'New team' }));
    expect(await screen.findByTestId('agent-team-edit-dialog')).toBeInTheDocument();
  });

  it('opens New team when the page asks (the side panel\'s button) and shows the header actions', async () => {
    grant('models.teams.list', 'models.teams.create');
    server.use(http.get(TEAMS, () => HttpResponse.json({ rows: [TEAM_ROW], total: 1 })));
    const onCreatingChange = vi.fn();
    renderWithQueryAndTheme(
      <TeamsPanel
        projectId="1"
        onStarted={vi.fn()}
        creating
        onCreatingChange={onCreatingChange}
        headerActions={<span data-testid="teams-header-probe" />}
      />,
    );
    expect(await screen.findByTestId('agent-team-edit-dialog')).toBeInTheDocument();
    expect(screen.getByTestId('teams-header-probe')).toBeInTheDocument();
    await userEvent.click(screen.getByTestId('agent-teams-create'));
    expect(onCreatingChange).toHaveBeenCalledWith(true);
  });

  it('draws a member with an avatar through the page\'s renderer and the others as a monogram', async () => {
    grant('models.teams.list');
    // Application ids are per project: agent 31 of project 2 is not this project's agent 31.
    const elsewhere = { application_id: 31, project_id: 2, version_id: 51, name: 'Elsewhere' };
    server.use(http.get(TEAMS, () => HttpResponse.json({ rows: [{ ...TEAM_ROW, members: [...TEAM_ROW.members, elsewhere] }], total: 1 })));
    renderWithQueryAndTheme(<Panel onStarted={vi.fn()} renderMemberAvatar={(avatar) => <span data-testid="live-avatar-probe">{avatar.iconUrl}</span>} />);
    expect(await screen.findByTestId('live-avatar-probe')).toHaveTextContent('/media/mira.png');
    expect(screen.getAllByTestId('live-avatar-probe')).toHaveLength(1);
    const monograms = within(screen.getByTestId('agent-team-avatars-3')).getAllByTestId('monogram');
    expect(monograms.map((monogram) => monogram.textContent)).toEqual(['CD', 'E']);
  });

  it('reads nothing without the list permission', async () => {
    grant('models.chat.conversations.create');
    const list = vi.fn();
    server.use(http.get(TEAMS, () => { list(); return HttpResponse.json({ rows: [], total: 0 }); }));
    renderWithQueryAndTheme(<Panel onStarted={vi.fn()} />);
    expect(await screen.findByTestId('agent-teams-forbidden')).toBeInTheDocument();
    expect(list).not.toHaveBeenCalled();
  });

  // Issue 3137: Publish to Marketplace in a team card's menu, offered with the agent publish permission.
  it('publishes a team and names the members that must be listed first', async () => {
    grant('models.teams.list', 'models.applications.publish.post');
    let published: string | undefined;
    server.use(
      http.get(TEAMS, () => HttpResponse.json({ rows: [TEAM_ROW], total: 1 })),
      http.post(`${BASE}/elitea_core/marketplace_team_publish/prompt_lib/:projectId/:teamId`, ({ params }) => {
        published = String(params['teamId']);
        return HttpResponse.json(
          { error: 'team_members_unlisted', msg: 'Publish these agents first', members: [{ position: 2, name: 'CI Doctor' }] },
          { status: 409 },
        );
      }),
    );
    renderWithQueryAndTheme(<Panel onStarted={vi.fn()} />);
    await userEvent.click(await screen.findByTestId('agent-team-menu-3'));
    await userEvent.click(await screen.findByTestId('agent-team-publish-3'));
    expect(await screen.findByTestId('agent-teams-publish-error')).toHaveTextContent(
      'Publish these agents to the Marketplace first: CI Doctor.',
    );
    expect(published).toBe('3');
  });

  it('offers no Publish without the publish permission', async () => {
    grant('models.teams.list');
    server.use(http.get(TEAMS, () => HttpResponse.json({ rows: [TEAM_ROW], total: 1 })));
    renderWithQueryAndTheme(<Panel onStarted={vi.fn()} />);
    expect(await screen.findByTestId('agent-team-name-3')).toBeInTheDocument();
    await userEvent.click(screen.getByTestId('agent-team-menu-3'));
    expect(await screen.findByTestId('agent-team-export-3')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-team-publish-3')).not.toBeInTheDocument();
  });
});

describe('SaveAsTeamDialog', () => {
  it('saves the conversation as a team under the given name', async () => {
    grant('models.teams.create');
    let body: unknown;
    let from = '';
    server.use(
      http.post(FROM, async ({ request, params }) => {
        from = String(params['conversationId']);
        body = await request.json();
        return HttpResponse.json(TEAM_ROW, { status: 201 });
      }),
    );
    const onClose = vi.fn();
    renderWithQueryAndTheme(<SaveAsTeamDialog projectId="1" conversationId="12" defaultName="Release 1.62" open onClose={onClose} />);
    const name = await screen.findByTestId('agent-team-save-as-name');
    expect(name).toHaveValue('Release 1.62');
    await userEvent.clear(name);
    await userEvent.type(name, 'Crew');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(body).toEqual({ name: 'Crew', description: '' }));
    expect(from).toBe('12');
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it('renders nothing while closed', () => {
    renderWithQueryAndTheme(<SaveAsTeamDialog projectId="1" conversationId="12" defaultName="x" open={false} onClose={vi.fn()} />);
    expect(screen.queryByTestId('agent-team-save-as-name')).not.toBeInTheDocument();
  });
});
