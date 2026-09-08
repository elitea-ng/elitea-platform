import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { getListPublicApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { Latest } from './Latest';
import { renderAgentsRoute } from './__tests__/testRouter';

function apps(names: readonly string[]) {
  return {
    rows: names.map((name, index) => ({
      project_id: 'public',
      id: String(index + 1),
      name,
      description: `${name} description`,
      version_id: 'v1',
      version_name: 'base',
      agent_type: 'classic',
      meta: null,
      tags: [],
      likes: 0,
      is_liked: false,
    })),
    total: names.length,
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('Latest', () => {
  it('shows the empty state when there are no public agents', async () => {
    server.use(getListPublicApplicationsMockHandler(apps([])));
    renderAgentsRoute(<Latest />);

    expect(await screen.findByText('No public agents yet.')).toBeInTheDocument();
  });

  it('renders every fetched agent', async () => {
    server.use(getListPublicApplicationsMockHandler(apps(['Agent One', 'Agent Two'])));
    renderAgentsRoute(<Latest />);

    expect(await screen.findByText('Agent One')).toBeInTheDocument();
    expect(screen.getByText('Agent Two')).toBeInTheDocument();
  });

  it('filters rows client-side by the search box', async () => {
    server.use(getListPublicApplicationsMockHandler(apps(['Agent One', 'Other Agent'])));
    const user = userEvent.setup();
    renderAgentsRoute(<Latest />);

    await screen.findByText('Agent One');
    await user.type(screen.getByPlaceholderText('Search'), 'One');

    await waitFor(() => expect(screen.queryByText('Other Agent')).not.toBeInTheDocument());
    expect(screen.getByText('Agent One')).toBeInTheDocument();
  });

  it('shows the "found nothing" empty state when a search matches no rows', async () => {
    server.use(getListPublicApplicationsMockHandler(apps(['Agent One'])));
    const user = userEvent.setup();
    renderAgentsRoute(<Latest />);

    await screen.findByText('Agent One');
    await user.type(screen.getByPlaceholderText('Search'), 'zzz');

    expect(await screen.findByText('No agents found.')).toBeInTheDocument();
  });

  it('navigates to the agent detail route when a row is clicked', async () => {
    server.use(getListPublicApplicationsMockHandler(apps(['Agent One'])));
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<Latest />);

    await user.click(await screen.findByText('Agent One'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/latest/1'));
  });
});
