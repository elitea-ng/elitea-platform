import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { getListApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { ApplicationList } from '@/shared/api/generated/model';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';

import { ApplicationsPanel } from './ApplicationsPanel';

/**
 * `ApplicationsPanel` now calls `useNavigate()` unconditionally (A12-ui
 * adversarial-review fix, finding 2 — see `ApplicationsPanel.tsx`'s own doc
 * comment), which throws without a real `<RouterProvider>` ancestor even on
 * a render that never clicks anything — mirrors the harness
 * `AllStuffPanel.test.tsx`/`UserPublicPage.test.tsx` already use for the
 * same reason. Also returns the `router` instance so a click test can
 * assert `router.state.location.pathname` — the root route below renders
 * `<ApplicationsPanel>` unconditionally (no `<Outlet />`/child routes), so
 * a DOM assertion alone cannot distinguish a real navigation from a no-op.
 */
function withQueryClient(ui: ReactNode) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => <QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>,
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
  });
  return { ...renderWithTheme(<RouterProvider router={router} />), router };
}

const EMPTY_LIST: ApplicationList = { rows: [], total: 0, page: 1, page_size: 20, total_pages: 0 };

describe('ApplicationsPanel', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('renders an UnavailablePanel in Public viewMode, without fetching', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    withQueryClient(
      <ApplicationsPanel
        projectId="pub-1"
        authorId="author-1"
        authorName="Ada"
        statuses={[]}
        forPipeline={false}
        isPublicProject
        enabled
      />,
    );
    // `withQueryClient` now mounts through a real `<RouterProvider>` (see its
    // own doc comment) — the router's initial route resolution is async, so
    // this can no longer assert synchronously (mirrors
    // `AllStuffPanel.test.tsx`'s identical `findByText` for its own
    // `isPublicProject` test).
    expect(await screen.findByText('This list is not available yet.')).toBeInTheDocument();
  });

  it('shows the author-specific empty state once the owner-mode fetch resolves with no matches', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getListApplicationsMockHandler(EMPTY_LIST));
    withQueryClient(
      <ApplicationsPanel
        projectId="proj-1"
        authorId="author-1"
        authorName="Ada"
        statuses={[]}
        forPipeline={false}
        isPublicProject={false}
        enabled
      />,
    );
    expect(await screen.findByText('Ada has not created agent yet.')).toBeInTheDocument();
  });

  it('uses the pipeline-specific empty copy when forPipeline is true', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getListApplicationsMockHandler(EMPTY_LIST));
    withQueryClient(
      <ApplicationsPanel
        projectId="proj-1"
        authorId="author-1"
        authorName="Ada"
        statuses={[]}
        forPipeline
        isPublicProject={false}
        enabled
      />,
    );
    expect(await screen.findByText('Ada has not created pipeline yet.')).toBeInTheDocument();
  });

  it('renders fetched items when the tab is enabled and owner-mode resolves with data', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      getListApplicationsMockHandler({
        rows: [
          {
            id: 'app-1',
            name: 'Research Agent',
            created_at: '2026-01-01T00:00:00Z',
            updated_at: '2026-01-01T00:00:00Z',
            owner_id: 'author-1',
            is_forked: false,
            meta: null,
            has_interrupt: false,
            tags: [],
          },
        ],
        total: 1,
        page: 1,
        page_size: 20,
        total_pages: 1,
      }),
    );
    withQueryClient(
      <ApplicationsPanel
        projectId="proj-1"
        authorId="author-1"
        authorName="Ada"
        statuses={[]}
        forPipeline={false}
        isPublicProject={false}
        enabled
      />,
    );
    await waitFor(() => expect(screen.getByText('Research Agent')).toBeInTheDocument());
  });

  it('navigates to the item’s agent detail page when its card is clicked (adversarial-review fix, cluster A12-ui, finding 2)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      getListApplicationsMockHandler({
        rows: [
          {
            id: 'app-1',
            name: 'Research Agent',
            created_at: '2026-01-01T00:00:00Z',
            updated_at: '2026-01-01T00:00:00Z',
            owner_id: 'author-1',
            is_forked: false,
            meta: null,
            has_interrupt: false,
            tags: [],
          },
        ],
        total: 1,
        page: 1,
        page_size: 20,
        total_pages: 1,
      }),
    );
    const { router } = withQueryClient(
      <ApplicationsPanel
        projectId="proj-1"
        authorId="author-1"
        authorName="Ada"
        statuses={[]}
        forPipeline={false}
        isPublicProject={false}
        enabled
      />,
    );
    await waitFor(() => expect(screen.getByText('Research Agent')).toBeInTheDocument());

    screen.getByText('Research Agent').click();
    await waitFor(() => expect(router.state.location.pathname).toBe('/user-public/agents/app-1'));
  });

  it('navigates to the item’s pipeline detail page when its card is clicked and forPipeline is true', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      getListApplicationsMockHandler({
        rows: [
          {
            id: 'pipe-1',
            name: 'Research Pipeline',
            created_at: '2026-01-01T00:00:00Z',
            updated_at: '2026-01-01T00:00:00Z',
            owner_id: 'author-1',
            is_forked: false,
            meta: null,
            has_interrupt: false,
            tags: [],
            agent_type: 'pipeline',
          },
        ],
        total: 1,
        page: 1,
        page_size: 20,
        total_pages: 1,
      }),
    );
    const { router } = withQueryClient(
      <ApplicationsPanel
        projectId="proj-1"
        authorId="author-1"
        authorName="Ada"
        statuses={[]}
        forPipeline
        isPublicProject={false}
        enabled
      />,
    );
    await waitFor(() => expect(screen.getByText('Research Pipeline')).toBeInTheDocument());

    screen.getByText('Research Pipeline').click();
    await waitFor(() => expect(router.state.location.pathname).toBe('/user-public/pipelines/pipe-1'));
  });
});
