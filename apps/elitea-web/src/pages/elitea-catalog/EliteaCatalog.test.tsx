/**
 * The catalogue shell: does `?tab=` select the right BODY, and does clicking
 * a tab move the URL?
 *
 * The two bodies are asserted by their own markers — `AgentHub`'s
 * "Welcome to Agent HUB" `CategoryFilter` title and `PublicSkillsCatalog`'s
 * `public-skills-catalog` testid — rather than by mocking them out. A shell
 * test that mocks both children proves the shell renders SOMETHING, which is
 * exactly the failure mode this port exists to avoid.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { catalogTabFromSearch, EliteaCatalog } from './EliteaCatalog';

interface Harness {
  readonly router: { state: { location: { search: unknown } } };
}

async function renderCatalog(initialUrl: string): Promise<Harness> {
  const rootRoute = createRootRoute();
  const catalogRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/elitea-catalog',
    validateSearch: (search: Record<string, unknown>) => search,
    component: () => <EliteaCatalog />,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([catalogRoute]),
    history: createMemoryHistory({ initialEntries: [initialUrl] }),
  });
  await router.load();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return { router };
}

describe('catalogTabFromSearch', () => {
  it('defaults to agents when the param is absent', () => {
    expect(catalogTabFromSearch(undefined)).toBe('agents');
  });

  it('selects skills only for the exact literal', () => {
    expect(catalogTabFromSearch('skills')).toBe('skills');
  });

  it('falls back to agents for an unknown value rather than rendering nothing', () => {
    expect(catalogTabFromSearch('nonsense')).toBe('agents');
  });
});

describe('EliteaCatalog shell', () => {
  it('renders the agents body by default', async () => {
    await renderCatalog('/elitea-catalog');
    expect(await screen.findByText('Welcome to Agent HUB')).toBeInTheDocument();
    expect(screen.queryByTestId('public-skills-catalog')).not.toBeInTheDocument();
  });

  it('leaves the page name to the shell title row', async () => {
    await renderCatalog('/elitea-catalog');
    await screen.findByTestId('catalog-agents-tab');
    expect(screen.queryByText('Welcome to ELITEA Catalog!')).toBeNull();
  });

  it('renders the skills body when ?tab=skills', async () => {
    await renderCatalog('/elitea-catalog?tab=skills');
    expect(await screen.findByTestId('public-skills-catalog')).toBeInTheDocument();
    expect(screen.queryByText('Welcome to Agent HUB')).not.toBeInTheDocument();
  });

  it('moves the URL to ?tab=skills when the Skills tab is clicked', async () => {
    const user = userEvent.setup();
    const { router } = await renderCatalog('/elitea-catalog');
    await user.click(screen.getByTestId('catalog-skills-tab'));
    await waitFor(() => {
      expect((router.state.location.search as { tab?: string }).tab).toBe('skills');
    });
    expect(await screen.findByTestId('public-skills-catalog')).toBeInTheDocument();
  });
});
