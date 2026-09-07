import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { getListApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { ApplicationList } from '@/shared/api/generated/model';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';
import { AGENTS_TAB_ADMIN_PERMISSION } from '../lib/constants';

import { AllStuffPanel, type AllStuffPanelProps } from './AllStuffPanel';

type Auth = { getSelectedProjectId: () => string | undefined; getUser: () => { permissions?: readonly string[] } | undefined };

/**
 * `AllStuffPanel` reads the viewer's permissions via
 * `useCurrentUserPermissions` (`../api/useRouterAuth`), which requires a
 * real `<RouterProvider>` ancestor (`useRouteContext` → `useMatch` →
 * `useRouter()` throws without one) — mirrors the render helper
 * `UserPublicPage.test.tsx` already uses for the same reason. Defaults to a
 * viewer who HAS the agents admin permission, matching this suite's
 * pre-existing (pre-fix) expectations; the permission-gate test below
 * overrides it.
 */
/**
 * Returns the `router` instance alongside the render result so tests can
 * assert `router.state.location.pathname`/`.params` after a click — the
 * A12-ui adversarial-review fix (finding 2) wires each card's `onSelect` to
 * a real `useNavigate()` call (see `AllStuffPanel.tsx`'s `navigateToEntity`
 * doc), and this harness's own `rootRoute` renders `<AllStuffPanel>`
 * unconditionally (no `<Outlet />`/child routes), so a DOM assertion alone
 * cannot tell a real navigation apart from a no-op — the router's own
 * location state is the only observable signal.
 */
function renderPanel(props: AllStuffPanelProps, auth: Auth = { getSelectedProjectId: () => props.projectId, getUser: () => ({ permissions: [AGENTS_TAB_ADMIN_PERMISSION] }) }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <AllStuffPanel {...props} />
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth },
  });
  return { ...renderWithTheme(<RouterProvider router={router} />), router };
}

const APP: ApplicationList['rows'][number] = {
  id: 'app-1',
  name: 'Classic Agent',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  owner_id: 'author-1',
  is_forked: false,
  meta: null,
  has_interrupt: false,
  tags: [],
  agent_type: 'classic',
};
const PIPELINE: ApplicationList['rows'][number] = {
  id: 'pipe-1',
  name: 'A Pipeline',
  created_at: '2026-02-01T00:00:00Z',
  updated_at: '2026-02-01T00:00:00Z',
  owner_id: 'author-1',
  is_forked: false,
  meta: null,
  has_interrupt: false,
  tags: [],
  agent_type: 'pipeline',
};

function mockHandlerDiscriminatingByAgentsType() {
  return getListApplicationsMockHandler((info) => {
    const url = new URL(info.request.url);
    const isPipeline = url.searchParams.get('agents_type') === 'pipeline';
    return {
      rows: isPipeline ? [PIPELINE] : [APP],
      total: 1,
      page: 1,
      page_size: 20,
      total_pages: 1,
    };
  });
}

describe('AllStuffPanel', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('renders an UnavailablePanel in Public viewMode', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    renderPanel({
      projectId: 'pub-1',
      authorId: 'author-1',
      authorName: 'Ada',
      statuses: [],
      isPublicProject: true,
      enabled: true,
    });
    expect(await screen.findByText('This list is not available yet.')).toBeInTheDocument();
  });

  it('merges applications and pipelines, newest first', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(mockHandlerDiscriminatingByAgentsType());
    renderPanel({
      projectId: 'proj-1',
      authorId: 'author-1',
      authorName: 'Ada',
      statuses: [],
      isPublicProject: false,
      enabled: true,
    });
    await waitFor(() => expect(screen.getByText('A Pipeline')).toBeInTheDocument());
    expect(screen.getByText('Classic Agent')).toBeInTheDocument();
    const headings = screen.getAllByRole('heading', { level: 3 });
    expect(headings.map((h) => h.textContent)).toEqual(['A Pipeline', 'Classic Agent']);
  });

  it('shows the all-stuff empty message when nothing matches', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getListApplicationsMockHandler({ rows: [], total: 0, page: 1, page_size: 20, total_pages: 0 }));
    renderPanel({
      projectId: 'proj-1',
      authorId: 'author-1',
      authorName: 'Ada',
      statuses: [],
      isPublicProject: false,
      enabled: true,
    });
    expect(await screen.findByText('Ada has not created anything yet.')).toBeInTheDocument();
  });

  it('excludes applications from the merge without the agents admin permission (adversarial-review fix, cluster A12-lib, finding 1)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(mockHandlerDiscriminatingByAgentsType());
    renderPanel(
      {
        projectId: 'proj-1',
        authorId: 'author-1',
        authorName: 'Ada',
        statuses: [],
        isPublicProject: false,
        enabled: true,
      },
      { getSelectedProjectId: () => 'proj-1', getUser: () => ({ permissions: ['some.unrelated.permission'] }) },
    );
    await waitFor(() => expect(screen.getByText('A Pipeline')).toBeInTheDocument());
    expect(screen.queryByText('Classic Agent')).not.toBeInTheDocument();
  });

  it('excludes applications from the merge when permissions have not loaded yet (empty permissions list)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(mockHandlerDiscriminatingByAgentsType());
    renderPanel(
      {
        projectId: 'proj-1',
        authorId: 'author-1',
        authorName: 'Ada',
        statuses: [],
        isPublicProject: false,
        enabled: true,
      },
      { getSelectedProjectId: () => 'proj-1', getUser: () => ({ permissions: [] }) },
    );
    // Zero permissions is `computeDisplayedTabs`'s own "not-yet-loaded" state
    // (every tab hidden, `displayed-tabs.test.ts`) — applications stay
    // excluded here too, same as the standalone Agents tab would.
    await waitFor(() => expect(screen.getByText('A Pipeline')).toBeInTheDocument());
    expect(screen.queryByText('Classic Agent')).not.toBeInTheDocument();
  });

  it('navigates to each item’s own detail page, by kind, when its card is clicked (adversarial-review fix, cluster A12-ui, finding 2)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(mockHandlerDiscriminatingByAgentsType());
    const { router } = renderPanel({
      projectId: 'proj-1',
      authorId: 'author-1',
      authorName: 'Ada',
      statuses: [],
      isPublicProject: false,
      enabled: true,
    });
    await waitFor(() => expect(screen.getByText('A Pipeline')).toBeInTheDocument());

    screen.getByText('Classic Agent').click();
    await waitFor(() => expect(router.state.location.pathname).toBe('/user-public/agents/app-1'));

    screen.getByText('A Pipeline').click();
    await waitFor(() => expect(router.state.location.pathname).toBe('/user-public/pipelines/pipe-1'));
  });
});
