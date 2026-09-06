import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { getListApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { PrivateAgentsList } from './PrivateAgentsList';
import { renderAgentsRoute } from './__tests__/testRouter';

function applications(rows: { id: string; name: string; status: string }[]) {
  return {
    rows: rows.map((row) => ({
      id: row.id,
      name: row.name,
      status: row.status,
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
      owner_id: 'user-1',
      is_forked: false,
      meta: null,
      has_interrupt: false,
    })),
    total: rows.length,
    page: 1,
    page_size: 20,
    total_pages: 1,
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('PrivateAgentsList', () => {
  it('shows every row on the "All" tab (statuses=undefined)', async () => {
    server.use(
      getListApplicationsMockHandler(
        applications([
          { id: '1', name: 'Draft App', status: 'draft' },
          { id: '2', name: 'Published App', status: 'published' },
        ]),
      ),
    );
    renderAgentsRoute(
      <PrivateAgentsList
        statuses={undefined}
        cardContentType="all"
      />,
      '/agents/all',
      { projectId: 'proj-1' },
    );

    expect(await screen.findByText('Draft App')).toBeInTheDocument();
    expect(screen.getByText('Published App')).toBeInTheDocument();
  });

  it('filters to only the requested statuses, client-side', async () => {
    server.use(
      getListApplicationsMockHandler(
        applications([
          { id: '1', name: 'Draft App', status: 'draft' },
          { id: '2', name: 'Published App', status: 'published' },
        ]),
      ),
    );
    renderAgentsRoute(
      <PrivateAgentsList
        statuses={['draft']}
        cardContentType="draft"
      />,
      '/agents/drafts',
      { projectId: 'proj-1' },
    );

    await waitFor(() => expect(screen.getByText('Draft App')).toBeInTheDocument());
    expect(screen.queryByText('Published App')).not.toBeInTheDocument();
  });

  it('shows the status-specific empty state when no row matches', async () => {
    server.use(getListApplicationsMockHandler(applications([])));
    renderAgentsRoute(
      <PrivateAgentsList
        statuses={['draft']}
        cardContentType="draft"
      />,
      '/agents/drafts',
      { projectId: 'proj-1' },
    );

    expect(await screen.findByText('No agents yet')).toBeInTheDocument();
  });

  it('filters client-side by the search box', async () => {
    server.use(
      getListApplicationsMockHandler(
        applications([
          { id: '1', name: 'Alpha App', status: 'draft' },
          { id: '2', name: 'Beta App', status: 'draft' },
        ]),
      ),
    );
    const user = userEvent.setup();
    renderAgentsRoute(
      <PrivateAgentsList
        statuses={undefined}
        cardContentType="all"
      />,
      '/agents/all',
      { projectId: 'proj-1' },
    );

    await screen.findByText('Alpha App');
    await user.type(screen.getByPlaceholderText('Search'), 'Alpha');

    await waitFor(() => expect(screen.queryByText('Beta App')).not.toBeInTheDocument());
  });

  it('sends the search box value as the real server-side `query` param, not just a client-side filter', async () => {
    const seenQueryValues: (string | null)[] = [];
    server.use(
      getListApplicationsMockHandler((info) => {
        seenQueryValues.push(new URL(info.request.url).searchParams.get('query'));
        return applications([{ id: '1', name: 'Alpha App', status: 'draft' }]);
      }),
    );
    const user = userEvent.setup();
    renderAgentsRoute(
      <PrivateAgentsList
        statuses={undefined}
        cardContentType="all"
      />,
      '/agents/all',
      { projectId: 'proj-1' },
    );

    await screen.findByText('Alpha App');
    await user.type(screen.getByPlaceholderText('Search'), 'Alpha');

    await waitFor(() => expect(seenQueryValues).toContain('Alpha'));
  });

  it('navigates to the agent detail route when a row is clicked, using the current :tab param', async () => {
    server.use(
      getListApplicationsMockHandler(applications([{ id: '7', name: 'My App', status: 'draft' }])),
    );
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(
      <PrivateAgentsList
        statuses={undefined}
        cardContentType="all"
      />,
      '/agents/all',
      { projectId: 'proj-1' },
    );

    await user.click(await screen.findByText('My App'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/all/7'));
  });
});
