/**
 * The hire wizard opened from a workforce manager's hire proposal (ADR-0051
 * decision 6, issue 1632): the version, the mode and the suggested binding are
 * filled in, nothing is created before the person clicks Hire, and the hire
 * request carries the proposal id.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import type { MarketplaceListingDetail } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { HireProposalWizard } from './HireProposalWizard';

const REQUIREMENTS = {
  items: [{ key: 'toolkit:github', kind: 'toolkit', type: 'github', name: 'Bot', tools: ['list_prs'], has_write_tools: false }],
  model_class: 'tool_calling',
  tier: 'low',
  has_write_tools: false,
};

const DETAIL = {
  listing: {
    id: '7',
    kind: 'agent',
    entity_id: '70',
    name: 'PR Reader',
    description: 'Reads pull requests.',
    category: 'Development',
    tags: [],
    publisher: { id: '3', name: 'Dana' },
    tier: 'low',
    current_version: { id: '311', name: 'v2.3', published_at: '2026-10-30T09:00:00Z' },
    hires: 4,
    rating_avg: 4.6,
    rating_count: 5,
    likes: 0,
    is_liked: false,
    featured: false,
    required_types: ['github'],
    has_write_tools: false,
    state: 'listed',
    hired: [],
  },
  welcome_message: '',
  conversation_starters: [],
  soul_summary: '',
  versions: [
    { id: '311', name: 'v2.3', state: 'current', release_notes: '', digest: 'abcdef0123456789', published_at: '2026-10-30T09:00:00Z', requirements: REQUIREMENTS },
    { id: '300', name: 'v2.0', state: 'available', release_notes: '', digest: 'abcdef0123456780', published_at: '2026-10-01T09:00:00Z', requirements: REQUIREMENTS },
  ],
  can_manage: false,
  can_moderate: false,
} as unknown as MarketplaceListingDetail;

function mount(): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <Outlet />
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({
        getParentRoute: () => rootRoute,
        path: '/',
        component: () => (
          <HireProposalWizard
            open
            projectId="5"
            listingId="7"
            proposal={{ proposalId: '9', versionId: '300', mode: 'fork', bindings: { 'toolkit:github': '12', 'toolkit:unknown': '99' } }}
            onClose={() => undefined}
          />
        ),
      }),
    ]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  } as never);
  renderWithTheme(<RouterProvider router={router} />);
}

describe('HireProposalWizard', () => {
  afterEach(() => resetGeneratedClient());

  it('fills in the proposal and settles it by hiring with its id', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const hires: unknown[] = [];
    server.use(
      http.get('/api/v2/elitea_core/marketplace_listing/prompt_lib/:projectId/:listingId', () => HttpResponse.json(DETAIL)),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({ rows: [{ id: '12', type: 'github', name: 'GitHub bot' }], total: 1 }),
      ),
      http.get('/api/v2/elitea_core/applications/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
      http.get('/api/v2/elitea_core/skills/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
      http.post('/api/v2/elitea_core/marketplace_hire/prompt_lib/:projectId', async ({ request }) => {
        hires.push(await request.json());
        return HttpResponse.json(
          { application_id: '88', version_id: '99', mode: 'fork', tools_enabled: true, awaiting_approval: false },
          { status: 201 },
        );
      }),
    );
    mount();
    const user = userEvent.setup();

    // Opens on the version the proposal names, not the current one.
    expect(await screen.findByTestId('hire-wizard')).toBeInTheDocument();
    await screen.findByTestId('hire-next');
    await user.click(screen.getByTestId('hire-next')); // mode
    await user.click(screen.getByTestId('hire-next')); // bindings: already chosen
    await user.click(screen.getByTestId('hire-next')); // review
    expect(hires).toEqual([]);
    await user.click(screen.getByTestId('hire-confirm'));
    await screen.findByTestId('hire-done');
    expect(hires).toEqual([
      {
        listing_id: '7',
        version_id: '300',
        mode: 'fork',
        bindings: [{ requirement_key: 'toolkit:github', toolkit_id: '12' }],
        proposal_id: '9',
      },
    ]);
  });

  it('says so when the listing is gone, and creates nothing', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.get('/api/v2/elitea_core/marketplace_listing/prompt_lib/:projectId/:listingId', () =>
        HttpResponse.json({ error: 'not found' }, { status: 404 }),
      ),
    );
    mount();
    expect(await screen.findByText(/not available any more/)).toBeInTheDocument();
    expect(screen.queryByTestId('hire-confirm')).not.toBeInTheDocument();
  });
});
