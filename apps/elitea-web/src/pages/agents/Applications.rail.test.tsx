import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  getGetAuthorDetailMockHandler,
  getListApplicationsMockHandler,
  getListPublicApplicationsMockHandler,
} from '@/shared/api/generated/applications/applications.msw';
import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getGetSocialTrendingAuthorsMockHandler } from '@/shared/api/generated/social/social.msw';
import { getListTagsMockHandler } from '@/shared/api/generated/tags/tags.msw';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { Applications } from './Applications';
import { renderAgentsRoute } from './__tests__/testRouter';

/**
 * The right-hand rail on `/agents/:tab` (`shared/ui/EntityRail`) — the
 * composition the baseline builds as `CardList -> RightPanel -> [SearchBar]
 * + Categories + AuthorInformation|TrendingAuthors`.
 */
const globals = globalThis as unknown as Record<string, unknown>;

function setConfig(publicProjectId: string): void {
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example',
    vite_base_uri: '/',
    vite_public_project_id: publicProjectId,
  };
  resetConfigForTests();
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    getListApplicationsMockHandler({ rows: [], total: 0, page: 1, page_size: 20, total_pages: 0 }),
    getListPublicApplicationsMockHandler({ rows: [], total: 0 }),
    getPermissionListMockHandler([]),
    getListTagsMockHandler({
      rows: [
        { id: 1, name: 'alpha', data: {} },
        { id: 2, name: 'beta', data: {} },
      ],
      total: 2,
    }),
    getGetSocialTrendingAuthorsMockHandler([{ id: '7', name: 'Grace Hopper', email: 'grace@example.com', avatar: '', likes: 12 }]),
    getGetAuthorDetailMockHandler({ id: 5, name: 'Ada Lovelace', total_applications: 7, public_applications: 3 }),
  );
});

afterEach(() => {
  resetGeneratedClient();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
});

describe('Applications right-hand rail', () => {
  it('renders the rail with the project tag chips', async () => {
    setConfig('1');
    renderAgentsRoute(<Applications />, '/agents/latest', { projectId: '1' });

    expect(await screen.findByTestId('entity-rail')).toBeInTheDocument();
    expect(await screen.findByTestId('tags-panel-chip-alpha')).toBeInTheDocument();
    expect(screen.getByTestId('tags-panel-chip-beta')).toBeInTheDocument();
  });

  it('writes the clicked tag into the `tags[]` search param and sorts it to the front', async () => {
    setConfig('1');
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<Applications />, '/agents/latest', { projectId: '1' });

    await user.click(await screen.findByTestId('tags-panel-chip-beta'));

    await waitFor(() => expect(router.state.location.search).toMatchObject({ 'tags[]': ['beta'] }));
    const chips = screen.getAllByRole('button').filter((element) => element.getAttribute('data-testid')?.startsWith('tags-panel-chip-'));
    expect(chips.map((chip) => chip.textContent)).toEqual(['beta', 'alpha']);
    expect(screen.getByTestId('tags-panel-chip-beta')).toHaveAttribute('aria-pressed', 'true');
  });

  it('clears the whole selection from the URL through the clear-all control', async () => {
    setConfig('1');
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<Applications />, '/agents/latest', { projectId: '1' });

    await user.click(await screen.findByTestId('tags-panel-chip-alpha'));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ 'tags[]': ['alpha'] }));

    await user.click(screen.getByTestId('tags-panel-clear-all'));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ 'tags[]': [] }));
    expect(screen.queryByTestId('tags-panel-clear-all')).toBeNull();
  });

  it('shows Trending Authors on the public feed tabs', async () => {
    setConfig('1');
    renderAgentsRoute(<Applications />, '/agents/latest', { projectId: '1', user: { id: '5', personal_project_id: '1' } });

    expect(await screen.findByText('Trending Authors')).toBeInTheDocument();
    expect(await screen.findByText('Grace Hopper')).toBeInTheDocument();
    expect(screen.queryByTestId('entity-rail-author')).toBeNull();
  });

  it('shows the author card with the /agents statistic on a private tab of the personal project', async () => {
    setConfig('1');
    renderAgentsRoute(<Applications />, '/agents/all', { projectId: '9', user: { id: '5', personal_project_id: '9' } });

    expect(await screen.findByTestId('entity-rail-author')).toBeInTheDocument();
    expect(screen.getByTestId('entity-rail-author-total')).toHaveTextContent('Agents:7');
    expect(screen.getByTestId('entity-rail-author-published')).toHaveTextContent('Published:3');
    expect(screen.queryByText('Trending Authors')).toBeNull();
  });

  it('falls back to Trending Authors when the selected project is NOT the viewer personal project', async () => {
    setConfig('1');
    renderAgentsRoute(<Applications />, '/agents/all', { projectId: '9', user: { id: '5', personal_project_id: '3' } });

    expect(await screen.findByText('Trending Authors')).toBeInTheDocument();
    expect(screen.queryByTestId('entity-rail-author')).toBeNull();
  });
});
