import type { ReactElement } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router';
import type { AnyRouter } from '@tanstack/react-router';
import { render } from '@testing-library/react';
import type { RenderResult } from '@testing-library/react';

import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { NavBlockerDialog } from '@/widgets/app-shell';

/**
 * Small, self-contained `pages/pipelines`-scoped router fixture — same
 * shape and same "not the real app tree" caveat as `pages/agents/__tests__/
 * testRouter.tsx` (Wave-2 unit A1g): covers exactly the three routes this
 * unit's own pages render at/navigate against (`/pipelines/$tab`,
 * `/pipelines/$tab/$agentId`, `/pipelines/create`), NOT `src/routes/**`
 * (outside this unit's ownership fence).
 */
/**
 * Whether to mount the app-wide unsaved-changes guard over the fixture.
 *
 * `AppShell` mounts `NavBlockerDialog` under EVERY real page, and it holds a
 * TanStack `useBlocker` that intercepts any pathname change while the guard
 * is raised. This fixture never mounted it, which is exactly why the unit
 * suite could not see that the pipeline editor's own post-save and
 * post-delete navigations were being blocked by the guard the page itself
 * armed. A test that wants to prove a navigation SURVIVES the guard has to
 * mount the thing that would block it.
 */
/**
 * `tags[]` as the REAL shell route parses it (`src/routes/-search/params.ts`
 * `list()` → `toStringArray`): always a string array, even when the URL
 * carries exactly one value. A fixture that returned the raw value handed
 * the page a bare string, which is a shape the real app never produces.
 */
function tagList(value: unknown): string[] {
  if (Array.isArray(value)) return value.map(String);
  if (typeof value === 'string' && value !== '') return [value];
  return [];
}

function buildTestRouter(
  initialPath: string,
  content: ReactElement,
  projectId: string | undefined,
  withSocket: boolean,
  withNavBlocker: boolean,
): AnyRouter {
  const rootRoute = createRootRoute({
    // `EditPipeline`'s real `<ConfigurationTab>` mount reads
    // `useSocketClient()` several layers down (`usePipelineChat`/
    // `usePipelineMCPToolsStatusMonitor`, and now `ChatBox` itself) — same
    // in-memory double `pages/toolkits/__tests__/testRouter.tsx` already
    // wraps its own tree with, for the identical reason.
    //
    // `withSocket=false` (`renderPipelinesRouteWithoutSocket`) is NOT a
    // reproduction of the real app tree, and the comment that used to say so
    // was wrong: `src/app/providers/AppProviders.tsx` mounts a real
    // `SocketClientContext.Provider` around every page. It is simply the
    // cheapest way to make the editor subtree throw on its first render, so
    // `EditPipeline.test.tsx` can assert `PipelineConfigurationTabBoundary`
    // contains that throw instead of the page unmounting.
    component: () => {
      const tree = (
        <>
          {withNavBlocker && <NavBlockerDialog />}
          <Outlet />
        </>
      );
      return withSocket ? (
        <SocketClientContext.Provider value={createTestSocketClient()}>{tree}</SocketClientContext.Provider>
      ) : (
        tree
      );
    },
  });

  const tabRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/pipelines/$tab',
    // `tags[]`/`author_id` are shell-wide in the real tree
    // (`src/routes/-search/common.ts`, mounted on `_shell/route.tsx`), so a
    // fixture that omitted them would silently DROP whatever the right-hand
    // rail writes and make a passing tag-selection test meaningless.
    validateSearch: (search: Record<string, unknown>) => ({
      sort_by: typeof search.sort_by === 'string' ? search.sort_by : undefined,
      sort_order: typeof search.sort_order === 'string' ? search.sort_order : undefined,
      author_id: typeof search.author_id === 'string' ? search.author_id : undefined,
      'tags[]': tagList(search['tags[]']),
    }),
    component: () => content,
  });

  const detailRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/pipelines/$tab/$agentId',
    validateSearch: (search: Record<string, unknown>) => ({
      isFromCreation: typeof search.isFromCreation === 'string' ? search.isFromCreation : undefined,
      history_run_id: typeof search.history_run_id === 'string' ? search.history_run_id : undefined,
    }),
    component: () => content,
  });

  // ROUTE-069 parity (`src/routes/_shell/pipelines/$tab.$agentId.$version.tsx`)
  // — the optional `/:version` child segment; `EditPipeline` reads
  // `version` via its own non-strict `useParams`, which picks it up from
  // whichever descendant route actually matched.
  const versionRoute = createRoute({
    getParentRoute: () => detailRoute,
    path: '$version',
    validateSearch: (search: Record<string, unknown>) => ({
      isFromCreation: typeof search.isFromCreation === 'string' ? search.isFromCreation : undefined,
      history_run_id: typeof search.history_run_id === 'string' ? search.history_run_id : undefined,
    }),
  });

  const createRoute_ = createRoute({
    getParentRoute: () => rootRoute,
    path: '/pipelines/create',
    component: () => content,
  });

  // Where `ChatWithPipelineButton` lands — the destination itself is outside
  // this unit (the real page is `processes/chat`'s), so the fixture renders
  // nothing there: a test asserts on `router.state.location.pathname` alone.
  // Same shape `pages/agents/__tests__/testRouter.tsx` gained for its twin.
  const chatRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/chat/$conversationId',
    component: () => null,
  });

  const routeTree = rootRoute.addChildren([tabRoute, detailRoute.addChildren([versionRoute]), createRoute_, chatRoute]);

  return createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialPath] }),
    context: { auth: { getSelectedProjectId: () => projectId } },
  });
}

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function createTestQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
      mutations: { retry: false },
    },
  });
}

export interface RenderPipelinesRouteResult extends RenderResult {
  readonly router: AnyRouter;
  readonly queryClient: QueryClient;
}

export interface RenderPipelinesRouteOptions {
  readonly queryClient?: QueryClient;
  readonly projectId?: string;
  /** Mount `widgets/app-shell`'s `NavBlockerDialog` over the fixture — see `buildTestRouter`. */
  readonly withNavBlocker?: boolean;
}

function renderWithTestRouter(
  content: ReactElement,
  initialPath: string,
  options: RenderPipelinesRouteOptions,
  withSocket: boolean,
): RenderPipelinesRouteResult {
  const queryClient = options.queryClient ?? createTestQueryClient();
  const router = buildTestRouter(initialPath, content, options.projectId, withSocket, options.withNavBlocker ?? false);

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

  return { ...view, router, queryClient };
}

export function renderPipelinesRoute(
  content: ReactElement,
  initialPath = '/pipelines/latest',
  options: RenderPipelinesRouteOptions = {},
): RenderPipelinesRouteResult {
  return renderWithTestRouter(content, initialPath, options, true);
}

/**
 * Same as `renderPipelinesRoute`, but WITHOUT a `SocketClientContext.Provider`
 * — reproduces this app's real, current, un-fixed gap (see `buildTestRouter`'s
 * own doc comment) so a test can assert `PipelineConfigurationTabBoundary`'s
 * fallback fires instead of the page crashing.
 */
export function renderPipelinesRouteWithoutSocket(
  content: ReactElement,
  initialPath = '/pipelines/latest',
  options: RenderPipelinesRouteOptions = {},
): RenderPipelinesRouteResult {
  return renderWithTestRouter(content, initialPath, options, false);
}
