import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { getListPublicApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { Latest } from './Latest';
import { renderPipelinesRoute } from './__tests__/testRouter';

function apps(rows: readonly { name: string; agentType: string }[]) {
  return {
    rows: rows.map((row, index) => ({
      project_id: 'public',
      id: String(index + 1),
      name: row.name,
      description: `${row.name} description`,
      version_id: 'v1',
      version_name: 'base',
      agent_type: row.agentType,
      meta: null,
      tags: [],
      likes: 0,
      is_liked: false,
    })),
    total: rows.length,
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('Latest', () => {
  it('shows the empty state when there are no public pipelines', async () => {
    server.use(getListPublicApplicationsMockHandler(apps([])));
    renderPipelinesRoute(<Latest />);

    expect(await screen.findByText('No public pipelines yet.')).toBeInTheDocument();
  });

  it('renders only rows whose agent_type is "pipeline", filtering out agents client-side', async () => {
    server.use(
      getListPublicApplicationsMockHandler(
        apps([
          { name: 'Pipeline One', agentType: 'pipeline' },
          { name: 'Classic Agent', agentType: 'classic' },
        ]),
      ),
    );
    renderPipelinesRoute(<Latest />);

    expect(await screen.findByText('Pipeline One')).toBeInTheDocument();
    expect(screen.queryByText('Classic Agent')).not.toBeInTheDocument();
  });

  it('filters rows client-side by the search box', async () => {
    server.use(
      getListPublicApplicationsMockHandler(
        apps([
          { name: 'Pipeline One', agentType: 'pipeline' },
          { name: 'Other Pipeline', agentType: 'pipeline' },
        ]),
      ),
    );
    const user = userEvent.setup();
    renderPipelinesRoute(<Latest />);

    await screen.findByText('Pipeline One');
    await user.type(screen.getByPlaceholderText('Search'), 'One');

    await waitFor(() => expect(screen.queryByText('Other Pipeline')).not.toBeInTheDocument());
    expect(screen.getByText('Pipeline One')).toBeInTheDocument();
  });

  it('shows the "found nothing" empty state when a search matches no rows', async () => {
    server.use(getListPublicApplicationsMockHandler(apps([{ name: 'Pipeline One', agentType: 'pipeline' }])));
    const user = userEvent.setup();
    renderPipelinesRoute(<Latest />);

    await screen.findByText('Pipeline One');
    await user.type(screen.getByPlaceholderText('Search'), 'zzz');

    expect(await screen.findByText('No pipelines found.')).toBeInTheDocument();
  });

  it('navigates to the pipeline detail route when a row is clicked', async () => {
    server.use(getListPublicApplicationsMockHandler(apps([{ name: 'Pipeline One', agentType: 'pipeline' }])));
    const user = userEvent.setup();
    const { router } = renderPipelinesRoute(<Latest />);

    await user.click(await screen.findByText('Pipeline One'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/latest/1'));
  });
});
