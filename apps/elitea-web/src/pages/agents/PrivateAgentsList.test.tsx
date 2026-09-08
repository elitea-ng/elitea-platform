import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { getListApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { PrivateAgentsList } from './PrivateAgentsList';
import { renderAgentsRoute } from './__tests__/testRouter';

function applications(rows: { id: string; name: string; status: string; tags?: string[] }[]) {
  return {
    rows: rows.map((row) => ({
      id: row.id,
      name: row.name,
      status: row.status,
      tags: row.tags ?? [],
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

  /*
   * The search box moved to the page HEADER (`widgets/page-header`'s
   * `ListSearchField`, mounted by `Applications.tsx`/`Pipelines.tsx`), so this
   * tab body no longer renders one. Its contract is now "read the route's
   * `query` param", which is what these tests drive. The typing interaction
   * itself is covered by `widgets/page-header/ui/ListSearchField.test.tsx`.
   */
  it('filters client-side by the header search box\'s `query` param', async () => {
    server.use(
      getListApplicationsMockHandler(
        applications([
          { id: '1', name: 'Alpha App', status: 'draft' },
          { id: '2', name: 'Beta App', status: 'draft' },
        ]),
      ),
    );
    renderAgentsRoute(
      <PrivateAgentsList
        statuses={undefined}
        cardContentType="all"
      />,
      '/agents/all?query=Alpha',
      { projectId: 'proj-1' },
    );

    expect(await screen.findByText('Alpha App')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText('Beta App')).not.toBeInTheDocument());
  });

  it('sends the `query` param on to the server, not just to the client-side filter', async () => {
    const seenQueryValues: (string | null)[] = [];
    server.use(
      getListApplicationsMockHandler((info) => {
        seenQueryValues.push(new URL(info.request.url).searchParams.get('query'));
        return applications([{ id: '1', name: 'Alpha App', status: 'draft' }]);
      }),
    );
    renderAgentsRoute(
      <PrivateAgentsList
        statuses={undefined}
        cardContentType="all"
      />,
      '/agents/all?query=Alpha',
      { projectId: 'proj-1' },
    );

    await screen.findByText('Alpha App');
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

  /**
   * The rail's tag selection is a REAL server-side filter now (issue 841).
   *
   * The applications repository ignored the `tags` request param and left
   * every row's `tags` empty, so the rail chips narrowed nothing and this
   * page disclosed them as decorative. Both halves are real, so this asserts
   * the param actually leaves the browser: a client-side filter over the
   * capped first page would narrow one page instead of the project.
   */
  it('sends the rail tag selection as the server-side `tags` param', async () => {
    const seenTagValues: (string | null)[] = [];
    server.use(
      getListApplicationsMockHandler((info) => {
        seenTagValues.push(new URL(info.request.url).searchParams.get('tags'));
        return applications([{ id: '1', name: 'Tagged App', status: 'draft' }]);
      }),
    );
    renderAgentsRoute(
      <PrivateAgentsList
        statuses={undefined}
        cardContentType="all"
      />,
      '/agents/all?tags%5B%5D=finance&tags%5B%5D=legal',
      { projectId: 'proj-1' },
    );

    await screen.findByText('Tagged App');
    await waitFor(() => expect(seenTagValues).toContain('finance,legal'));
  });

  it('omits the `tags` param when no rail chip is selected', async () => {
    const seenTagValues: (string | null)[] = [];
    server.use(
      getListApplicationsMockHandler((info) => {
        seenTagValues.push(new URL(info.request.url).searchParams.get('tags'));
        return applications([{ id: '1', name: 'Plain App', status: 'draft' }]);
      }),
    );
    renderAgentsRoute(
      <PrivateAgentsList
        statuses={undefined}
        cardContentType="all"
      />,
      '/agents/all',
      { projectId: 'proj-1' },
    );

    await screen.findByText('Plain App');
    expect(seenTagValues).toEqual([null]);
  });

  it('renders the tags the server puts on a list row', async () => {
    server.use(
      getListApplicationsMockHandler(applications([{ id: '1', name: 'Tagged App', status: 'draft', tags: ['finance', 'legal'] }])),
    );
    renderAgentsRoute(
      <PrivateAgentsList
        statuses={undefined}
        cardContentType="all"
      />,
      '/agents/all',
      { projectId: 'proj-1' },
    );

    expect(await screen.findByText('finance')).toBeInTheDocument();
    expect(screen.getByText('legal')).toBeInTheDocument();
  });
});
