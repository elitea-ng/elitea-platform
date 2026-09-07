import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { getGetAgentCategoriesMockHandler, getGetPublicApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../test/setup';

import AgentHub from './AgentHub';

const ROWS = [
  {
    project_id: '1',
    id: 'app-1',
    name: 'Research Agent',
    description: '',
    version_id: 'v-1',
    version_name: 'v1',
    agent_type: 'agent',
    meta: { category: 'Productivity' },
    tags: [],
    likes: 0,
    is_liked: false,
  },
  {
    project_id: '1',
    id: 'app-2',
    name: 'Support Bot',
    description: '',
    version_id: 'v-2',
    version_name: 'v1',
    agent_type: 'agent',
    meta: { category: 'Support' },
    tags: [],
    likes: 0,
    is_liked: false,
  },
];

/**
 * A stand-in for the catalogue handler that actually READS its parameters,
 * because the real one does now (issue #36 item 8).
 *
 *  - Trending (`sort_by=likes`) and My Liked (`my_liked=true`) hit the same
 *    URL as the bulk page, so they answer empty here; otherwise every agent
 *    name would appear three times in the DOM and `findByText` would rightly
 *    fail on ambiguity.
 *  - `?query=` filters over name and description, exactly as the handler's
 *    ILIKE pair does. A mock that ignored `query` would let a client-side
 *    filter pass this test — which is the bug this whole change removes.
 */
function mockAgentsHubData(): void {
  server.use(
    getGetAgentCategoriesMockHandler({
      categories: [
        { name: 'Productivity', is_default: true },
        { name: 'Support', is_default: true },
      ],
      total: 2,
    }),
  );
  server.use(
    // Full `/api/v2` path and the real top-level `{rows, total}` body.
    // The `*` wildcard used to hide the raw-fetch URL bug in
    // `useAgentHubData.ts`. The `data` wrapper used to hide the envelope
    // misread in the same file.
    http.get('/api/v2/elitea_core/public_applications/prompt_lib', ({ request }) => {
      const params = new URL(request.url).searchParams;
      const isBulk = params.get('sort_by') !== 'likes' && !params.has('my_liked');
      if (!isBulk) return HttpResponse.json({ rows: [], total: 0 }, { status: 200 });
      const needle = (params.get('query') ?? '').toLowerCase();
      const rows = needle === ''
        ? ROWS
        : ROWS.filter(row =>
            row.name.toLowerCase().includes(needle) || row.description.toLowerCase().includes(needle),
          );
      return HttpResponse.json({ rows, total: rows.length }, { status: 200 });
    }),
  );
  server.use(getGetPublicApplicationMockHandler());
}

/** Real `<RouterProvider>` (search params + `useNavigate` inside `AgentModal`) + `<QueryClientProvider>`, mirroring `pages/user-public/ui/ApplicationsPanel.test.tsx`'s own harness. `initialSearch` seeds the `?agentId=` deep link (finding 8). */
function withProviders(ui: ReactNode, initialSearch = '') {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({
    validateSearch: (search: Record<string, unknown>) => ({ agentId: typeof search['agentId'] === 'string' ? search['agentId'] : undefined }),
    component: () => <QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>,
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: [`/${initialSearch}`] }),
  });
  return { ...renderWithTheme(<RouterProvider router={router} />), router };
}

describe('AgentHub', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('renders both categories with their agents once fetched', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockAgentsHubData();

    withProviders(<AgentHub />);

    expect(await screen.findByText('Research Agent')).toBeInTheDocument();
    expect(screen.getByText('Support Bot')).toBeInTheDocument();
    // Both the category-filter chip and the section heading render the
    // category name as text, so these are scoped by role to stay
    // unambiguous.
    expect(screen.getByRole('heading', { name: 'Productivity' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Support' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Productivity' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Support' })).toBeInTheDocument();
  });

  it('sends the search box text to the server and renders what comes back (issue #36 item 8)', async () => {
    // The typed text reaches the handler as `?query=`. The mock filters the
    // way the handler does, so a client-side filter would no longer be able
    // to make this pass on its own: the rows it would filter never arrive.
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockAgentsHubData();
    const user = userEvent.setup();

    withProviders(<AgentHub />);
    await screen.findByText('Research Agent');

    await user.type(screen.getByPlaceholderText('Search for agents'), 'support');

    await waitFor(
      () => { expect(screen.queryByText('Research Agent')).not.toBeInTheDocument(); },
      { timeout: 5000 },
    );
    expect(screen.getByText('Support Bot')).toBeInTheDocument();
  });

  it('offers the three sort choices and asks the server for the chosen one', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const sorts: string[] = [];
    mockAgentsHubData();
    server.use(
      http.get('/api/v2/elitea_core/public_applications/prompt_lib', ({ request }) => {
        const params = new URL(request.url).searchParams;
        if (params.get('sort_by') === 'likes' || params.has('my_liked')) {
          return HttpResponse.json({ rows: [], total: 0 }, { status: 200 });
        }
        sorts.push(`${params.get('sort_by')}:${params.get('sort_order')}`);
        return HttpResponse.json({ rows: ROWS, total: ROWS.length }, { status: 200 });
      }),
    );
    const user = userEvent.setup();

    withProviders(<AgentHub />);
    await screen.findByText('Research Agent');
    await waitFor(() => { expect(sorts).toContain('created_at:desc'); });

    await user.click(screen.getByRole('combobox', { name: 'Sort by' }));
    await user.click(await screen.findByRole('option', { name: 'Name A-Z' }));

    await waitFor(() => { expect(sorts).toContain('name:asc'); }, { timeout: 5000 });
  });

  it('filters down to only the selected category when its chip is clicked (finding 9 — previously dead tag-filter plumbing)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockAgentsHubData();
    const user = userEvent.setup();

    withProviders(<AgentHub />);
    await screen.findByText('Research Agent');

    await user.click(screen.getByRole('button', { name: 'Support' }));

    await waitFor(() => expect(screen.queryByText('Research Agent')).not.toBeInTheDocument());
    expect(screen.getByText('Support Bot')).toBeInTheDocument();
  });

  it('shows a no-results message when the search matches nothing', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockAgentsHubData();
    const user = userEvent.setup();

    withProviders(<AgentHub />);
    await screen.findByText('Research Agent');

    await user.type(screen.getByPlaceholderText('Search for agents'), 'nonexistent agent');

    expect(await screen.findByText('No agents found', undefined, { timeout: 5000 })).toBeInTheDocument();
  });

  /*
   * DEFECT this guards: the hub reports a refused list through
   * `useAgentHubData`'s `error`. No component reads it. The page shows
   * "No agents found". A broken hub then looks like an empty catalogue.
   */
  it('shows a load error instead of the empty state when the list request is refused', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      getGetAgentCategoriesMockHandler({
        categories: [{ name: 'Productivity', is_default: true }],
        total: 1,
      }),
    );
    server.use(
      http.get('/api/v2/elitea_core/public_applications/prompt_lib', () =>
        HttpResponse.json({ error: 'forbidden' }, { status: 403 }),
      ),
    );

    withProviders(<AgentHub />);

    expect(
      await screen.findByText('The agent list did not load. Reload the page to try again.'),
    ).toBeInTheDocument();
    expect(screen.queryByText('No agents found')).not.toBeInTheDocument();
  });

  it('auto-opens the deep-linked agent\'s modal once it appears in the fetched data (adversarial-review fix, finding 8)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockAgentsHubData();

    withProviders(<AgentHub />, '?agentId=app-2');

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Support Bot')).toBeInTheDocument();
    expect(within(dialog).getByText('Start conversation')).toBeInTheDocument();
  });

  it('does not open any modal when there is no agentId in the URL', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockAgentsHubData();

    withProviders(<AgentHub />);

    await screen.findByText('Research Agent');
    expect(screen.queryByText('Start conversation')).not.toBeInTheDocument();
  });
});
