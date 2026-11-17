import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import type { MarketplaceListingSummary } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { SidePanelHarness } from '@/test/sidePanelHarness';

import { server } from '../../test/setup';

import { Marketplace } from './Marketplace';

function row(id: string, name: string, overrides: Partial<MarketplaceListingSummary> = {}): MarketplaceListingSummary {
  return {
    id,
    kind: 'agent',
    entity_id: `e${id}`,
    name,
    description: `${name} does things`,
    category: 'Development',
    tags: [],
    publisher: { id: '3', name: 'Dana Owner' },
    tier: 'low',
    current_version: { id: `v${id}`, name: 'v1', published_at: '2026-10-30T09:00:00Z' },
    hires: 0,
    rating_avg: null,
    rating_count: 0,
    likes: 0,
    is_liked: false,
    featured: false,
    required_types: [],
    has_write_tools: false,
    state: 'listed',
    hired: [],
    ...overrides,
  };
}

const ROWS = [
  row('1', 'Research Agent', { hires: 9, featured: true }),
  row('2', 'Support Bot', {
    category: 'Support',
    required_types: ['github'],
    hired: [{ application_id: '70', mode: 'install', version_id: 'v2', version_name: 'v1' }],
  }),
  row('3', 'Release Notes Writer', { hires: 3 }),
];
const SKILL = row('9', 'PR Reviewer', { kind: 'skill' });

const FACETS = {
  kinds: [{ value: 'agent', label: 'Agents', count: 3 }],
  categories: [
    { value: 'Development', label: 'Development', count: 2 },
    { value: 'Support', label: 'Support', count: 1 },
  ],
  tags: [],
  publishers: [{ value: '3', label: 'Dana Owner', count: 3 }],
  tiers: [{ value: 'low', label: 'low', count: 3 }],
  required_types: [{ value: 'github', label: 'github', count: 1 }],
  hired_here: 1,
};

let requests: URL[] = [];

/** A search stand-in that READS the query it is given, as the route does: a mock that ignored it would let a client-side filter pass. */
function mockSearch(pageSize = 100): void {
  requests = [];
  server.use(
    http.get('/api/v2/elitea_core/marketplace_listings/prompt_lib/:projectId', ({ request }) => {
      const url = new URL(request.url);
      requests.push(url);
      const params = url.searchParams;
      let pool = params.get('kind') === 'skill' ? [SKILL] : ROWS;
      const needle = (params.get('query') ?? '').toLowerCase();
      pool = pool.filter((candidate) => candidate.name.toLowerCase().includes(needle));
      const category = params.get('category');
      if (category !== null) pool = pool.filter((candidate) => category.split(',').includes(candidate.category));
      if (params.get('featured') === 'true') pool = pool.filter((candidate) => candidate.featured);
      if (params.get('hired_here') === 'true') pool = pool.filter((candidate) => candidate.hired.length > 0);
      if (params.get('sort') === 'hires') pool = [...pool].sort((a, b) => b.hires - a.hires);
      const offset = Number(params.get('offset') ?? 0);
      const limit = Math.min(Number(params.get('limit') ?? pageSize), pageSize);
      return HttpResponse.json({ rows: pool.slice(offset, offset + limit), total: pool.length, facets: FACETS });
    }),
  );
}

function mount(initial = '/marketplace'): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <SidePanelHarness toggle>
          <Outlet />
        </SidePanelHarness>
      </QueryClientProvider>
    ),
  });
  const page = (): ReactNode => <Marketplace />;
  const string = (value: unknown): string | undefined =>
    typeof value === 'string' || typeof value === 'number' ? String(value) : undefined;
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({
        getParentRoute: () => rootRoute,
        path: '/marketplace',
        validateSearch: (search: Record<string, unknown>) => ({
          kind: string(search['kind']),
          query: string(search['query']),
          category: string(search['category']),
          requires: string(search['requires']),
          hired_here: string(search['hired_here']),
          sort_by: string(search['sort_by']),
        }),
        component: page,
      }),
      createRoute({
        getParentRoute: () => rootRoute,
        path: '/marketplace/$kind/$listingId',
        component: () => <p data-testid="listing-page" />,
      }),
    ]),
    history: createMemoryHistory({ initialEntries: [initial] }),
    context: { auth: { getSelectedProjectId: () => '5' } },
  });
  renderWithTheme(<RouterProvider router={router} />);
}

describe('Marketplace', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('draws every listing as a card that links to its listing page, with publisher, hires and "No ratings yet"', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockSearch();
    mount();

    const all = await screen.findByTestId('marketplace-section-all');
    const link = within(all).getByRole('link', { name: /Research Agent/ });
    expect(link).toHaveAttribute('href', '/marketplace/agents/1');
    expect(within(all).getAllByTestId('marketplace-card')).toHaveLength(3);
    expect(within(all).getAllByText('No ratings yet')).toHaveLength(3);
    expect(within(all).getAllByText('by Dana Owner')).toHaveLength(3);
    expect(within(all).getByText('Requires github')).toBeInTheDocument();
    // Hired here: the badge names the version.
    expect(within(all).getByTestId('marketplace-card-hired')).toHaveTextContent('Hired v1');
  });

  it('draws a heart with the count on every card, beside its link and not inside it, skills included (skills epic S6)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.get('/api/v2/elitea_core/marketplace_listings/prompt_lib/:projectId', () =>
        HttpResponse.json({
          rows: [row('9', 'PR Reviewer', { kind: 'skill', entity_id: '501', likes: 6, is_liked: true })],
          total: 1,
          facets: FACETS,
        }),
      ),
    );
    mount('/marketplace?kind=skills');

    const card = (await screen.findAllByTestId('marketplace-card'))[0] as HTMLElement;
    const heart = within(card).getByRole('button', { name: 'Like PR Reviewer' });
    expect(heart).toHaveAttribute('aria-pressed', 'true');
    expect(within(heart).getByTestId('marketplace-like-count')).toHaveTextContent('6');
    // A button in an anchor is invalid, and a like must not open the listing.
    expect(within(card).getByRole('link', { name: /PR Reviewer/ })).not.toContainElement(heart);
  });

  it('asks for the Most liked sort by the server\'s key', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockSearch();
    mount('/marketplace?sort_by=likes');
    await screen.findByTestId('marketplace-section-all');
    expect(requests.some((url) => url.searchParams.get('sort') === 'likes')).toBe(true);
  });

  it('asks the server for one page and appends the next on Show more, not the whole catalogue', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockSearch(2);
    mount();
    const user = userEvent.setup();

    const all = await screen.findByTestId('marketplace-section-all');
    expect(within(all).getAllByTestId('marketplace-card')).toHaveLength(2);
    expect(
      requests.some((url) => url.searchParams.get('offset') === '0' && url.searchParams.get('limit') === '24'),
    ).toBe(true);
    await user.click(await screen.findByTestId('marketplace-show-more'));
    await waitFor(() => expect(within(all).getAllByTestId('marketplace-card')).toHaveLength(3));
    expect(screen.queryByTestId('marketplace-show-more')).not.toBeInTheDocument();
  });

  it('fills Featured from the editorial flag and Popular from hires, only while nothing is filtered', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockSearch();
    mount();
    const user = userEvent.setup();

    const featured = await screen.findByTestId('marketplace-section-featured');
    expect(within(featured).getAllByTestId('marketplace-card')).toHaveLength(1);
    expect(within(featured).getByText('Research Agent')).toBeInTheDocument();
    expect(requests.some((url) => url.searchParams.get('featured') === 'true')).toBe(true);
    expect(requests.some((url) => url.searchParams.get('sort') === 'hires')).toBe(true);

    await user.type(
      within(await screen.findByTestId('side-panel-tab-filters')).getByTestId('marketplace-search'),
      'release',
    );
    await waitFor(() => expect(screen.queryByTestId('marketplace-section-featured')).not.toBeInTheDocument());
    expect(screen.queryByTestId('marketplace-section-popular')).not.toBeInTheDocument();
    // The search is the SERVER's: it was sent, and the list is what came back.
    await waitFor(() => expect(requests.some((url) => url.searchParams.get('query') === 'release')).toBe(true));
    const all = await screen.findByTestId('marketplace-section-all');
    await waitFor(() => expect(within(all).getAllByTestId('marketplace-card')).toHaveLength(1));
    expect(within(all).getByText('Release Notes Writer')).toBeInTheDocument();
  });

  it('narrows by category from the server counts and lists each active facet as a removable chip when the sidebar is closed', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockSearch();
    mount();
    const user = userEvent.setup();

    const browse = await screen.findByTestId('side-panel-tab-filters');
    await user.click(await within(browse).findByRole('checkbox', { name: 'Support (1)' }));
    const all = await screen.findByTestId('marketplace-section-all');
    await waitFor(() => expect(within(all).getAllByTestId('marketplace-card')).toHaveLength(1));
    expect(requests.some((url) => url.searchParams.get('category') === 'Support')).toBe(true);

    await user.click(within(browse).getByRole('checkbox', { name: 'github (1)' }));
    await user.click(within(browse).getByTestId('marketplace-hired-here').querySelector('input') as HTMLInputElement);

    // The summary bar is for a CLOSED sidebar: close it, and every active filter is a chip there.
    await user.click(screen.getByTestId('side-panel-harness-toggle'));
    expect(await screen.findByText('Category: Support')).toBeInTheDocument();
    expect(screen.getByText('Requires: github')).toBeInTheDocument();
    expect(screen.getByText('Hired in this project')).toBeInTheDocument();
    await user.click(screen.getByTestId('marketplace-clear-all'));
    await waitFor(() => expect(screen.queryByText('Category: Support')).not.toBeInTheDocument());
    expect(await screen.findByTestId('marketplace-section-featured')).toBeInTheDocument();
  });

  it('keeps every facet in the URL, so a filtered view opens as it was left', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockSearch();
    mount('/marketplace?category=Support&requires=github&hired_here=1&sort_by=hires');

    await waitFor(() => expect(requests.length).toBeGreaterThan(0));
    const first = requests[0] as URL;
    expect(first.searchParams.get('category')).toBe('Support');
    expect(first.searchParams.get('requires')).toBe('github');
    expect(first.searchParams.get('hired_here')).toBe('true');
    expect(first.searchParams.get('sort')).toBe('hires');
  });

  it('switches to skills from the Browse sidebar and asks the route for skills', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockSearch();
    mount();
    const user = userEvent.setup();

    const browse = await screen.findByTestId('side-panel-tab-filters');
    await user.click(within(browse).getByRole('radio', { name: 'Skills' }));
    const all = await screen.findByTestId('marketplace-section-all');
    await waitFor(() =>
      expect(within(all).getByRole('link', { name: /PR Reviewer/ })).toHaveAttribute('href', '/marketplace/skills/9'),
    );
    expect(requests.some((url) => url.searchParams.get('kind') === 'skill')).toBe(true);

    await user.click(within(browse).getByRole('combobox', { name: 'Sort by' }));
    for (const name of ['Relevance', 'Most hired', 'Top rated', 'Newest first', 'Name A-Z']) {
      expect(screen.getByRole('option', { name })).toBeInTheDocument();
    }
  });

  it('opens on the skills for ?kind=skills', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockSearch();
    mount('/marketplace?kind=skills');

    const all = await screen.findByTestId('marketplace-section-all');
    expect(within(all).getByText('PR Reviewer')).toBeInTheDocument();
    // With the Browse sidebar open the summary bar has nothing to add and is not drawn; closed, it counts.
    expect(screen.queryByTestId('marketplace-summary-bar')).not.toBeInTheDocument();
    await userEvent.setup().click(screen.getByTestId('side-panel-harness-toggle'));
    expect(await screen.findByText('Skills: 1')).toBeInTheDocument();
    expect(screen.getByTestId('marketplace-open-browse')).toBeInTheDocument();
  });

  it('says nothing is published yet, not "no match", when the catalogue is empty and nothing is filtered', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.get('/api/v2/elitea_core/marketplace_listings/prompt_lib/:projectId', () =>
        HttpResponse.json({ rows: [], total: 0, facets: { ...FACETS, hired_here: 0 } }),
      ),
    );
    mount();

    expect(await screen.findByText('Nothing is published yet')).toBeInTheDocument();
    expect(screen.queryByText('No listings match')).not.toBeInTheDocument();
  });

  it('says what to do when nothing matches', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockSearch();
    mount();
    const user = userEvent.setup();

    await user.type(
      within(await screen.findByTestId('side-panel-tab-filters')).getByTestId('marketplace-search'),
      'zzz',
    );
    expect(await screen.findByText('No listings match')).toBeInTheDocument();
    expect(screen.getByText('Clear a filter or try different search terms.')).toBeInTheDocument();
  });

  it('reports a refused search instead of an empty marketplace', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.get('/api/v2/elitea_core/marketplace_listings/prompt_lib/:projectId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    mount();

    expect(await screen.findByText('The listings did not load. Reload the page to try again.')).toBeInTheDocument();
    expect(screen.queryByText('No listings match')).not.toBeInTheDocument();
  });
});
