import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { getGetAgentCategoriesMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { SidePanelHarness } from '@/test/sidePanelHarness';

import { server } from '../../test/setup';

import { Marketplace } from './Marketplace';

function row(id: string, name: string, category: string, likes: number, versionId: string) {
  return {
    project_id: '1',
    id,
    name,
    description: `${name} does things`,
    version_id: versionId,
    version_name: 'v1',
    agent_type: 'agent',
    meta: { category },
    tags: [],
    likes,
    is_liked: false,
  };
}

const ROWS = [
  row('app-1', 'Research Agent', 'Productivity', 5, '10'),
  row('app-2', 'Support Bot', 'Support', 0, '20'),
  row('app-3', 'Release Notes Writer', 'Productivity', 2, '30'),
];

const SKILLS = [
  { id: 41, name: 'PR Reviewer', description: 'Reviews pull requests', tags: ['Development'], versions: [{ id: 6, name: 'v1.0', status: 'published' }] },
  { id: 42, name: 'Draft Only', description: 'Not published', tags: [], versions: [{ id: 7, name: 'base', status: 'draft' }] },
];

let agentRequests: string[] = [];

/** A catalogue stand-in that READS `query`, as the real handler does: a mock that ignored it would let a client-side filter pass. */
function mockCatalogue(): void {
  agentRequests = [];
  server.use(
    getGetAgentCategoriesMockHandler({
      categories: [
        { name: 'Productivity', is_default: true },
        { name: 'Support', is_default: true },
      ],
      total: 2,
    }),
    http.get('/api/v2/elitea_core/public_applications/prompt_lib', ({ request }) => {
      agentRequests.push(request.url);
      const needle = (new URL(request.url).searchParams.get('query') ?? '').toLowerCase();
      const rows = ROWS.filter((candidate) => candidate.name.toLowerCase().includes(needle));
      return HttpResponse.json({ rows, total: rows.length });
    }),
    http.get('/api/v2/elitea_core/public_skills/prompt_lib', () => HttpResponse.json({ rows: SKILLS, total: SKILLS.length })),
  );
}

function mount(initial = '/marketplace'): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <SidePanelHarness>
          <Outlet />
        </SidePanelHarness>
      </QueryClientProvider>
    ),
  });
  const page = (): ReactNode => <Marketplace />;
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/marketplace', validateSearch: (search: Record<string, unknown>) => ({ kind: typeof search['kind'] === 'string' ? search['kind'] : undefined }), component: page }),
      createRoute({ getParentRoute: () => rootRoute, path: '/marketplace/$kind/$listingId', component: () => <p data-testid="listing-page" /> }),
    ]),
    history: createMemoryHistory({ initialEntries: [initial] }),
  });
  renderWithTheme(<RouterProvider router={router} />);
}

describe('Marketplace', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('draws every listing as a link to its listing page, with no per-category Show more', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockCatalogue();
    mount();

    const all = await screen.findByTestId('marketplace-section-all');
    const link = within(all).getByRole('link', { name: /Research Agent/ });
    expect(link).toHaveAttribute('href', '/marketplace/agents/app-1');
    expect(within(all).getAllByTestId('marketplace-card')).toHaveLength(3);
    expect(screen.queryByTestId('agent-category-show-more')).not.toBeInTheDocument();
    expect(screen.queryByText('Welcome to Agent HUB')).not.toBeInTheDocument();
  });

  it('shows the Featured and Popular rows only while no search or category is active', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockCatalogue();
    mount();
    const user = userEvent.setup();

    expect(await screen.findByTestId('marketplace-section-featured')).toBeInTheDocument();
    // Popular holds only the listings somebody liked: Support Bot has none.
    const popular = screen.getByTestId('marketplace-section-popular');
    expect(within(popular).queryByText('Support Bot')).not.toBeInTheDocument();
    expect(within(popular).getByText('Research Agent')).toBeInTheDocument();

    await user.type(within(await screen.findByTestId('side-panel-tab-filters')).getByTestId('marketplace-search'), 'release');
    await waitFor(() => expect(screen.queryByTestId('marketplace-section-featured')).not.toBeInTheDocument());
    expect(screen.queryByTestId('marketplace-section-popular')).not.toBeInTheDocument();
    // The search is the SERVER's: it was sent, and the list is what came back.
    await waitFor(() => expect(agentRequests.some((url) => new URL(url).searchParams.get('query') === 'release')).toBe(true));
    const all = await screen.findByTestId('marketplace-section-all');
    await waitFor(() => expect(within(all).getAllByTestId('marketplace-card')).toHaveLength(1));
    expect(within(all).getByText('Release Notes Writer')).toBeInTheDocument();
  });

  it('narrows by several categories at once and lists each active filter as a removable chip', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockCatalogue();
    mount();
    const user = userEvent.setup();

    const browse = await screen.findByTestId('side-panel-tab-filters');
    await user.click(await within(browse).findByRole('checkbox', { name: 'Support' }));
    const all = await screen.findByTestId('marketplace-section-all');
    expect(within(all).getAllByTestId('marketplace-card')).toHaveLength(1);
    expect(screen.getByText('Category: Support')).toBeInTheDocument();

    await user.click(within(browse).getByRole('checkbox', { name: 'Productivity' }));
    await waitFor(() => expect(within(all).getAllByTestId('marketplace-card')).toHaveLength(3));

    await user.click(screen.getByTestId('marketplace-clear-all'));
    await waitFor(() => expect(screen.queryByText('Category: Support')).not.toBeInTheDocument());
    expect(screen.getByTestId('marketplace-section-featured')).toBeInTheDocument();
  });

  it('switches to skills from the Browse sidebar: skills with a published version, and no "Most liked" sort', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockCatalogue();
    mount();
    const user = userEvent.setup();

    const browse = await screen.findByTestId('side-panel-tab-filters');
    await user.click(within(browse).getByRole('radio', { name: 'Skills' }));
    const all = await screen.findByTestId('marketplace-section-all');
    expect(within(all).getByRole('link', { name: /PR Reviewer/ })).toHaveAttribute('href', '/marketplace/skills/41');
    // A skill with no published version cannot be opened or added, so it is not listed.
    expect(within(all).queryByText('Draft Only')).not.toBeInTheDocument();
    expect(screen.queryByTestId('marketplace-section-popular')).not.toBeInTheDocument();

    await user.click(within(browse).getByRole('combobox', { name: 'Sort by' }));
    expect(screen.queryByRole('option', { name: 'Most liked' })).not.toBeInTheDocument();
    expect(screen.getByRole('option', { name: 'Name A-Z' })).toBeInTheDocument();
  });

  it('opens on the skills for ?kind=skills', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockCatalogue();
    mount('/marketplace?kind=skills');

    const all = await screen.findByTestId('marketplace-section-all');
    expect(within(all).getByText('PR Reviewer')).toBeInTheDocument();
    expect(screen.getByText('Skills: 1')).toBeInTheDocument();
  });

  it('says what to do when nothing matches', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockCatalogue();
    mount();
    const user = userEvent.setup();

    await user.type(within(await screen.findByTestId('side-panel-tab-filters')).getByTestId('marketplace-search'), 'zzz');
    expect(await screen.findByText('No listings match')).toBeInTheDocument();
    expect(screen.getByText('Clear a filter or try different search terms.')).toBeInTheDocument();
  });

  it('reports a refused list instead of an empty marketplace', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      getGetAgentCategoriesMockHandler({ categories: [], total: 0 }),
      http.get('/api/v2/elitea_core/public_applications/prompt_lib', () => HttpResponse.json({ error: 'boom' }, { status: 500 })),
    );
    mount();

    expect(await screen.findByText('The listings did not load. Reload the page to try again.')).toBeInTheDocument();
    expect(screen.queryByText('No listings match')).not.toBeInTheDocument();
  });
});
