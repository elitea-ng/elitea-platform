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

  /*
   * The search box moved to the page HEADER (`widgets/page-header`'s
   * `ListSearchField`, mounted by `Applications.tsx`/`Pipelines.tsx`), so this
   * tab body no longer renders one. Its contract is now "read the route's
   * `query` param", which is what these tests drive. The typing interaction
   * itself is covered by `widgets/page-header/ui/ListSearchField.test.tsx`.
   */
  it('filters rows client-side by the header search box\'s `query` param', async () => {
    server.use(
      getListPublicApplicationsMockHandler(
        apps([
          { name: 'Pipeline One', agentType: 'pipeline' },
          { name: 'Other Pipeline', agentType: 'pipeline' },
        ]),
      ),
    );
    renderPipelinesRoute(<Latest />, '/pipelines/latest?query=One');

    expect(await screen.findByText('Pipeline One')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText('Other Pipeline')).not.toBeInTheDocument());
  });

  it('shows the "found nothing" empty state when a search matches no rows', async () => {
    server.use(getListPublicApplicationsMockHandler(apps([{ name: 'Pipeline One', agentType: 'pipeline' }])));
    renderPipelinesRoute(<Latest />, '/pipelines/latest?query=zzz');

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
