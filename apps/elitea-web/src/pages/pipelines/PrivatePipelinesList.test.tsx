import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { getListApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { PrivatePipelinesList } from './PrivatePipelinesList';
import { renderPipelinesRoute } from './__tests__/testRouter';

function applications(rows: { id: string; name: string; status: string; tags?: string[] }[]) {
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
      tags: row.tags ?? [],
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

describe('PrivatePipelinesList', () => {
  it('shows every row, unfiltered by status (the baseline only ever renders this component with statuses=[All])', async () => {
    server.use(
      getListApplicationsMockHandler(
        applications([
          { id: '1', name: 'Draft Pipeline', status: 'draft' },
          { id: '2', name: 'Published Pipeline', status: 'published' },
        ]),
      ),
    );
    renderPipelinesRoute(
      <PrivatePipelinesList cardContentType="all" />,
      '/pipelines/all',
      { projectId: 'proj-1' },
    );

    expect(await screen.findByText('Draft Pipeline')).toBeInTheDocument();
    expect(screen.getByText('Published Pipeline')).toBeInTheDocument();
  });

  it('shows the empty state when there are no rows', async () => {
    server.use(getListApplicationsMockHandler(applications([])));
    renderPipelinesRoute(
      <PrivatePipelinesList cardContentType="all" />,
      '/pipelines/all',
      { projectId: 'proj-1' },
    );

    expect(await screen.findByText('No pipelines yet')).toBeInTheDocument();
  });

  it('filters client-side by the search box', async () => {
    server.use(
      getListApplicationsMockHandler(
        applications([
          { id: '1', name: 'Alpha Pipeline', status: 'draft' },
          { id: '2', name: 'Beta Pipeline', status: 'draft' },
        ]),
      ),
    );
    const user = userEvent.setup();
    renderPipelinesRoute(
      <PrivatePipelinesList cardContentType="all" />,
      '/pipelines/all',
      { projectId: 'proj-1' },
    );

    await screen.findByText('Alpha Pipeline');
    await user.type(screen.getByPlaceholderText('Search'), 'Alpha');

    await waitFor(() => expect(screen.queryByText('Beta Pipeline')).not.toBeInTheDocument());
  });

  it('navigates to the pipeline detail route when a row is clicked, using the current :tab param', async () => {
    server.use(getListApplicationsMockHandler(applications([{ id: '7', name: 'My Pipeline', status: 'draft' }])));
    const user = userEvent.setup();
    const { router } = renderPipelinesRoute(
      <PrivatePipelinesList cardContentType="all" />,
      '/pipelines/all',
      { projectId: 'proj-1' },
    );

    await user.click(await screen.findByText('My Pipeline'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/all/7'));
  });

  /** The same server-side tag filter `pages/agents` sends (issue 841). */
  it('sends the rail tag selection as the server-side `tags` param', async () => {
    const seenTagValues: (string | null)[] = [];
    server.use(
      getListApplicationsMockHandler((info) => {
        seenTagValues.push(new URL(info.request.url).searchParams.get('tags'));
        return applications([{ id: '1', name: 'Tagged Pipeline', status: 'draft', tags: ['finance'] }]);
      }),
    );
    renderPipelinesRoute(
      <PrivatePipelinesList cardContentType="all" />,
      '/pipelines/all?tags%5B%5D=finance',
      { projectId: 'proj-1' },
    );

    await screen.findByText('Tagged Pipeline');
    await waitFor(() => expect(seenTagValues).toContain('finance'));
    expect(screen.getByText('finance')).toBeInTheDocument();
  });
});
