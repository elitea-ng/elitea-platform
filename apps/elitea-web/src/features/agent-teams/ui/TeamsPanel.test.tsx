import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { renderWithQueryAndTheme } from '@/test/renderWithQueryAndTheme';

import { SaveAsTeamDialog } from './SaveAsTeamDialog';
import { TeamsPanel } from './TeamsPanel';

const BASE = '/api/v2';
const TEAMS = `${BASE}/elitea_core/agent_teams/prompt_lib/:projectId`;
const TEAM = `${BASE}/elitea_core/agent_team/prompt_lib/:projectId/:teamId`;
const START = `${BASE}/elitea_core/agent_team_conversation/prompt_lib/:projectId/:teamId`;
const FROM = `${BASE}/elitea_core/agent_team_from_conversation/prompt_lib/:projectId/:conversationId`;
const PERMISSIONS = `${BASE}/auth/permissions/prompt_lib/:projectId`;

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

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('TeamsPanel', () => {
  it('lists the teams, marks the lead, and offers only what the caller may do', async () => {
    grant('models.teams.list');
    server.use(http.get(TEAMS, () => HttpResponse.json({ rows: [TEAM_ROW], total: 1 })));
    renderWithQueryAndTheme(<TeamsPanel projectId="1" onStarted={vi.fn()} />);
    expect(await screen.findByTestId('agent-team-name-3')).toHaveTextContent('Release crew');
    expect(screen.getByText('Mira (lead)')).toBeInTheDocument();
    expect(screen.getByTestId('agent-team-start-3')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-teams-create')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agent-team-edit-3')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agent-team-delete-3')).not.toBeInTheDocument();
  });

  // #1847: past one page the list says how many there are and loads the rest.
  it('pages past the first 200 teams', async () => {
    grant('models.teams.list');
    const offsets: string[] = [];
    const row = (id: number) => ({ ...TEAM_ROW, id, name: `Team ${id}` });
    server.use(
      http.get(TEAMS, ({ request }) => {
        const offset = Number(new URL(request.url).searchParams.get('offset') ?? '0');
        offsets.push(String(offset));
        const rows = offset === 0 ? Array.from({ length: 200 }, (_, i) => row(i + 1)) : [row(201)];
        return HttpResponse.json({ rows, total: 201 });
      }),
    );
    renderWithQueryAndTheme(<TeamsPanel projectId="1" onStarted={vi.fn()} />);
    expect(await screen.findByTestId('agent-teams-count')).toHaveTextContent('Showing 200 of 201 teams');
    expect(screen.queryByTestId('agent-team-name-201')).not.toBeInTheDocument();
    await userEvent.click(screen.getByTestId('agent-teams-more'));
    expect(await screen.findByTestId('agent-team-name-201')).toHaveTextContent('Team 201');
    expect(screen.queryByTestId('agent-teams-more')).not.toBeInTheDocument();
    expect(offsets).toEqual(['0', '200']);
  });

  it('offers no more when every team is shown', async () => {
    grant('models.teams.list');
    server.use(http.get(TEAMS, () => HttpResponse.json({ rows: [TEAM_ROW], total: 1 })));
    renderWithQueryAndTheme(<TeamsPanel projectId="1" onStarted={vi.fn()} />);
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
    renderWithQueryAndTheme(<TeamsPanel projectId="1" onStarted={onStarted} />);
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
    renderWithQueryAndTheme(<TeamsPanel projectId="1" onStarted={vi.fn()} />);
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
    renderWithQueryAndTheme(<TeamsPanel projectId="1" onStarted={vi.fn()} />);
    await userEvent.click(await screen.findByTestId('agent-team-delete-3'));
    await userEvent.click(await screen.findByRole('button', { name: /delete|confirm/i }));
    expect(await screen.findByTestId('agent-teams-empty')).toBeInTheDocument();
  });

  it('reads nothing without the list permission', async () => {
    grant('models.chat.conversations.create');
    const list = vi.fn();
    server.use(http.get(TEAMS, () => { list(); return HttpResponse.json({ rows: [], total: 0 }); }));
    renderWithQueryAndTheme(<TeamsPanel projectId="1" onStarted={vi.fn()} />);
    expect(await screen.findByTestId('agent-teams-forbidden')).toBeInTheDocument();
    expect(list).not.toHaveBeenCalled();
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
