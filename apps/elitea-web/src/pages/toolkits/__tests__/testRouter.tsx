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

/**
 * Small, self-contained `pages/toolkits`-scoped router fixture — same shape
 * and same "not the real app tree" caveat as `pages/agents/__tests__/
 * testRouter.tsx`/`pages/apps/__tests__/testRouter.tsx`: covers exactly the
 * routes this unit's own pages read params from (`/toolkits/create[/:toolkitType]`,
 * `/toolkits/:tab/:toolkitId`), NOT `src/routes/**` (outside this unit's
 * ownership fence, and — as of this unit's own build — those route files
 * still render `RouteShell` only; see `CreateToolkit.tsx`'s own doc
 * comment).
 */
function buildTestRouter(initialPath: string, content: ReactElement, projectId: string | undefined): AnyRouter {
  const rootRoute = createRootRoute({
    // `ToolkitForm`'s own tree reads `useSocketClient()` (something inside
    // `ToolBase`'s dynamic-discovery UI) — same in-memory double
    // `ToolkitEditor.test.tsx`/`ToolkitTypeSelector.test.tsx` (this same
    // unit) already wrap their own trees with.
    component: () => (
      <SocketClientContext.Provider value={createTestSocketClient()}>
        <Outlet />
      </SocketClientContext.Provider>
    ),
  });

  const createRoute_ = createRoute({
    getParentRoute: () => rootRoute,
    path: '/toolkits/create',
    component: () => content,
  });

  const createTypeRoute = createRoute({
    getParentRoute: () => createRoute_,
    path: '$toolkitType',
    component: () => content,
  });

  const editRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/toolkits/$tab/$toolkitId',
    component: () => content,
  });

  const createMcpRoute = createRoute({ getParentRoute: () => rootRoute, path: '/mcps/create', component: () => content });
  const createMcpTypeRoute = createRoute({ getParentRoute: () => createMcpRoute, path: '$toolkitType', component: () => content });
  const routeTree = rootRoute.addChildren([createRoute_.addChildren([createTypeRoute]), editRoute, createMcpRoute.addChildren([createMcpTypeRoute])]);

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

export interface RenderToolkitsRouteResult extends RenderResult {
  readonly router: AnyRouter;
  readonly queryClient: QueryClient;
}

export function renderToolkitsRoute(
  content: ReactElement,
  initialPath = '/toolkits/create',
  options: { queryClient?: QueryClient; projectId?: string } = {},
): RenderToolkitsRouteResult {
  const queryClient = options.queryClient ?? createTestQueryClient();
  const router = buildTestRouter(initialPath, content, options.projectId);

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
