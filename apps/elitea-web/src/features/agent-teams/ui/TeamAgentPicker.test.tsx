/**
 * #1846: the team editor's agent picker searches the project's agents on the
 * server, so an agent past the first page can still be found and added.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { renderWithQueryAndTheme } from '@/test/renderWithQueryAndTheme';

import { draftOf } from '../lib/teamDraft';
import { TeamAgentPicker } from './TeamAgentPicker';

const BASE = '/api/v2';
const APPLICATIONS = `${BASE}/elitea_core/applications/prompt_lib/:projectId`;

const agent = (id: number, name: string) => ({ id: String(id), project_id: '1', name });

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('TeamAgentPicker', () => {
  it('searches past the first page and adds the match', async () => {
    const queries: (string | null)[] = [];
    server.use(
      http.get(APPLICATIONS, ({ request }) => {
        const query = new URL(request.url).searchParams.get('query');
        queries.push(query);
        if (query === 'Zed') return HttpResponse.json({ rows: [agent(250, 'Zed')], total: 1 });
        return HttpResponse.json({ rows: [agent(1, 'Alpha'), agent(2, 'Beta')], total: 250 });
      }),
    );
    const onPick = vi.fn();
    renderWithQueryAndTheme(<TeamAgentPicker projectId="1" draft={draftOf(undefined)} disabled={false} onPick={onPick} />);
    expect(await screen.findByText('Showing 2 of 250 agents. Type to narrow the list.')).toBeInTheDocument();
    await userEvent.type(screen.getByTestId('agent-team-edit-add'), 'Zed');
    await userEvent.click(await screen.findByTestId('agent-team-edit-add-250'));
    expect(onPick).toHaveBeenCalledWith(expect.objectContaining({ id: '250', name: 'Zed' }));
    expect(queries).toContain('Zed');
    await waitFor(() => expect(screen.getByTestId('agent-team-edit-add')).toHaveValue(''));
  });

  it('leaves out an agent already on the team', async () => {
    server.use(http.get(APPLICATIONS, () => HttpResponse.json({ rows: [agent(1, 'Alpha'), agent(2, 'Beta')], total: 2 })));
    const draft = { ...draftOf(undefined), members: [{ application_id: 1, project_id: 1, version_id: 5, name: 'Alpha' }] };
    renderWithQueryAndTheme(<TeamAgentPicker projectId="1" draft={draft} disabled={false} onPick={vi.fn()} />);
    await userEvent.click(screen.getByTestId('agent-team-edit-add'));
    expect(await screen.findByTestId('agent-team-edit-add-2')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-team-edit-add-1')).not.toBeInTheDocument();
    expect(screen.queryByText(/Type to narrow the list/)).not.toBeInTheDocument();
  });
});
