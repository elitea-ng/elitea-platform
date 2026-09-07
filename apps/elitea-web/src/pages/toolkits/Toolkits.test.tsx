import type { ReactElement } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import type { AnyRouter } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getGetPlatformSettingsMockHandler, getGetPlatformSettingsResponseMock } from '@/shared/api/generated/admin/admin.msw';
import { getListToolkitInstancesMockHandler } from '@/shared/api/generated/toolkits/toolkits.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { Toolkits, shouldRedirectToCreatePage } from './Toolkits';

/**
 * Small, self-contained `pages/toolkits`-scoped router fixture covering
 * exactly the routes THIS page (`Toolkits`) reads params from / navigates
 * to: `/toolkits/$tab`, `/toolkits/$tab/$toolkitId`, `/toolkits/create`, and
 * the `/mcps/**` siblings. `./__tests__/testRouter.tsx` (this same unit's
 * `EditToolkit`/`CreateToolkit` fixture) does not register `/toolkits/$tab`
 * or any `/mcps/**` route at all — not reused here, a fresh local fixture
 * instead (same "not the real app tree" caveat every sibling `__tests__/
 * testRouter.tsx` in this codebase already documents).
 */
function buildTestRouter(initialPath: string, content: ReactElement, projectId: string | undefined): AnyRouter {
  const rootRoute = createRootRoute({ component: () => <Outlet /> });

  const toolkitsTabRoute = createRoute({ getParentRoute: () => rootRoute, path: '/toolkits/$tab', component: () => content });
  const toolkitsDetailRoute = createRoute({ getParentRoute: () => rootRoute, path: '/toolkits/$tab/$toolkitId' });
  const toolkitsCreateRoute = createRoute({ getParentRoute: () => rootRoute, path: '/toolkits/create' });
  const mcpsTabRoute = createRoute({ getParentRoute: () => rootRoute, path: '/mcps/$tab', component: () => content });
  const mcpsDetailRoute = createRoute({ getParentRoute: () => rootRoute, path: '/mcps/$tab/$mcpId' });
  const mcpsCreateRoute = createRoute({ getParentRoute: () => rootRoute, path: '/mcps/create' });

  const routeTree = rootRoute.addChildren([
    toolkitsTabRoute,
    toolkitsDetailRoute,
    toolkitsCreateRoute,
    mcpsTabRoute,
    mcpsDetailRoute,
    mcpsCreateRoute,
  ]);

  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { auth: { getSelectedProjectId: () => projectId } },
  });
}

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function createTestQueryClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } } });
}

function renderToolkitsPage(content: ReactElement, initialPath: string, projectId: string | undefined) {
  const queryClient = createTestQueryClient();
  const router = buildTestRouter(initialPath, content, projectId);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider
        theme={theme}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return { ...view, router };
}

const globals = globalThis as unknown as Record<string, unknown>;

/** Same `elitea_ui_config` global + `resetConfigForTests` pattern `pages/agents/Applications.test.tsx` already establishes for exercising `isPublicProject`-gated behaviour. */
function setPublicProjectConfig(publicProjectId: string): void {
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example',
    vite_base_uri: '/',
    vite_public_project_id: publicProjectId,
  };
  resetConfigForTests();
}

function toolkitRow(overrides: Partial<Record<string, unknown>> = {}) {
  return {
    id: 'tk-1',
    type: 'github',
    name: 'My GitHub',
    description: '',
    settings: {},
    meta: {},
    created_at: '2026-01-01T00:00:00Z',
    author_id: 1,
    ...overrides,
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  // `ToolkitsList`'s own `useIsMcpVisible()` reads this real platform-wide
  // toggle (`GET /platform_settings`); the GENERATED default mock returns
  // `mcp_enabled: faker.datatype.boolean()` — a coin flip. Left unmocked,
  // roughly half of all runs would have `isMcpVisible` resolve `false` and
  // silently filter every MCP-typed row back out of the rendered list,
  // flaking the MCP-scoped tests below. Forced `true` (permissive) here so
  // this file's own real `isMCP` scoping (this page's OWN filter, upstream
  // of that toggle) is what every assertion is actually exercising.
  //
  // `mcp_in_menu_enabled` is pinned for EXACTLY the same reason, and is the
  // second half of the same toggle (A14, issue 200): `useIsMcpVisible` now
  // reads both flags, as the baseline always did, and the generated mock
  // randomises this one too — `faker.helpers.arrayElement([boolean,
  // undefined])`, so it is a coin flip that lands on `false` about a third of
  // the time. Pinning only `mcp_enabled` left that second coin flip live, and
  // it flaked this file's MCP-scoped tests until it was found.
  server.use(
    getGetPlatformSettingsMockHandler(
      getGetPlatformSettingsResponseMock({ mcp_enabled: true, mcp_in_menu_enabled: true }),
    ),
  );
});

afterEach(() => {
  resetGeneratedClient();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
});

describe('shouldRedirectToCreatePage (pure)', () => {
  const base = {
    isPublicProject: false,
    isLoading: false,
    isError: false,
    hasMoreRawPages: false,
    scopedItemCount: 0,
    selectedTypesCount: 0,
  };

  it('is true when every gate passes (private project, not loading, no error, no more pages, zero scoped items, no type filter)', () => {
    expect(shouldRedirectToCreatePage(base)).toBe(true);
  });

  it('is false for the public project', () => {
    expect(shouldRedirectToCreatePage({ ...base, isPublicProject: true })).toBe(false);
  });

  it('is false while loading', () => {
    expect(shouldRedirectToCreatePage({ ...base, isLoading: true })).toBe(false);
  });

  it('is false on error', () => {
    expect(shouldRedirectToCreatePage({ ...base, isError: true })).toBe(false);
  });

  it('is false while more raw pages remain unloaded', () => {
    expect(shouldRedirectToCreatePage({ ...base, hasMoreRawPages: true })).toBe(false);
  });

  it('is false with a non-zero scoped item count', () => {
    expect(shouldRedirectToCreatePage({ ...base, scopedItemCount: 5 })).toBe(false);
  });

  it('is false with an active type filter', () => {
    expect(shouldRedirectToCreatePage({ ...base, selectedTypesCount: 1 })).toBe(false);
  });
});

describe('Toolkits', () => {
  it('renders the real, fetched toolkit cards (regression: R1 — used to render a permanently-blank placeholder Box with no data fetch at all)', async () => {
    server.use(getListToolkitInstancesMockHandler({ rows: [toolkitRow({ id: 'tk-1', name: 'My GitHub', type: 'github' })], total: 1 }));

    renderToolkitsPage(<Toolkits />, '/toolkits/all', 'proj-1');

    expect(await screen.findByText('Toolkits')).toBeInTheDocument();
    expect(await screen.findByText('My GitHub')).toBeInTheDocument();
    expect(screen.getByTestId('toolkit-card')).toBeInTheDocument();
  });

  it('scopes to non-MCP rows on the /toolkits surface, hiding MCP-typed rows', async () => {
    server.use(
      getListToolkitInstancesMockHandler({
        rows: [toolkitRow({ id: 'tk-1', name: 'My GitHub', type: 'github' }), toolkitRow({ id: 'tk-2', name: 'My MCP Server', type: 'mcp' })],
        total: 2,
      }),
    );

    renderToolkitsPage(<Toolkits />, '/toolkits/all', 'proj-1');

    expect(await screen.findByText('My GitHub')).toBeInTheDocument();
    expect(screen.queryByText('My MCP Server')).not.toBeInTheDocument();
  });

  it('scopes to MCP-typed rows on the /mcps surface (isMCP), hiding non-MCP rows', async () => {
    server.use(
      getListToolkitInstancesMockHandler({
        rows: [toolkitRow({ id: 'tk-1', name: 'My GitHub', type: 'github' }), toolkitRow({ id: 'tk-2', name: 'My MCP Server', type: 'mcp' })],
        total: 2,
      }),
    );

    renderToolkitsPage(<Toolkits isMCP />, '/mcps/all', 'proj-1');

    expect(await screen.findByText('MCPs')).toBeInTheDocument();
    expect(await screen.findByText('My MCP Server')).toBeInTheDocument();
    expect(screen.queryByText('My GitHub')).not.toBeInTheDocument();
  });

  it('navigates to the toolkit detail route when a card is clicked', async () => {
    server.use(getListToolkitInstancesMockHandler({ rows: [toolkitRow({ id: 'tk-7', name: 'My GitHub', type: 'github' })], total: 1 }));
    const user = userEvent.setup();

    const { router } = renderToolkitsPage(<Toolkits />, '/toolkits/all', 'proj-1');

    await user.click(await screen.findByText('My GitHub'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/toolkits/all/tk-7'));
  });

  it('shows the create CTA (not the plain "Nothing found" placeholder) for a genuinely empty PUBLIC project, and does not auto-redirect away from the list', async () => {
    setPublicProjectConfig('proj-1');
    server.use(getListToolkitInstancesMockHandler({ rows: [], total: 0 }));

    const { router } = renderToolkitsPage(<Toolkits />, '/toolkits/all', 'proj-1');

    expect(await screen.findByText('No toolkits yet')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/toolkits/all');
  });

  it('auto-redirects a genuinely empty PRIVATE project straight to the Create page (regression: R1 — the old blank-placeholder page never fetched data, so this redirect could never fire)', async () => {
    server.use(getListToolkitInstancesMockHandler({ rows: [], total: 0 }));

    const { router } = renderToolkitsPage(<Toolkits />, '/toolkits/all', 'proj-1');

    await waitFor(() => expect(router.state.location.pathname).toBe('/toolkits/create'));
  });

  it('renders the MCPs title and list-panel test hook when isMCP is true', async () => {
    server.use(getListToolkitInstancesMockHandler({ rows: [], total: 0 }));
    setPublicProjectConfig('proj-1');

    renderToolkitsPage(<Toolkits isMCP />, '/mcps/all', 'proj-1');

    expect(await screen.findByText('MCPs')).toBeInTheDocument();
    expect(screen.getByTestId('mcps-list-panel')).toBeInTheDocument();
  });

  /** COMPOSITION ROOT — see `pages/agents/Applications.test.tsx` (issue 841). */
  it('renders its one tab inside the shared page header, on both surfaces', async () => {
    server.use(getListToolkitInstancesMockHandler({ rows: [], total: 0 }));
    renderToolkitsPage(<Toolkits />, '/toolkits/all', 'proj-1');

    const header = await screen.findByTestId('page-header');
    expect(header).toContainElement(screen.getByTestId('toolkits-tab-all'));
    expect(screen.getByRole('tablist', { name: 'Toolkits' })).toBeInTheDocument();
  });
});
