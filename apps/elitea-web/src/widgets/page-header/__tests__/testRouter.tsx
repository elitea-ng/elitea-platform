import type { ReactElement } from 'react';

import { ThemeProvider } from '@mui/material/styles';
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

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

/**
 * A one-route fixture for the two header controls that OWN a search param
 * (`ListSearchField` writes `query`, `ListViewToggle` writes `view`).
 *
 * Neither can be rendered by `renderWithTheme` — both call `useSearch`/
 * `useNavigate`, which throw outside a router. The route below parses the two
 * keys exactly as the real `src/routes/-search/params.ts` schemas do for these
 * values, so a test can assert on `router.state.location.search`.
 */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

export interface RenderHeaderRouteResult extends RenderResult {
  readonly router: AnyRouter;
}

export function renderHeaderRoute(content: ReactElement, initialPath = '/agents/all'): RenderHeaderRouteResult {
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const listRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/$entity/$tab',
    validateSearch: (search: Record<string, unknown>) => ({
      query: typeof search.query === 'string' ? search.query : '',
      view: typeof search.view === 'string' ? search.view : undefined,
    }),
    component: () => content,
  });

  const router = createRouter({
    routeTree: rootRoute.addChildren([listRoute]),
    history: createMemoryHistory({ initialEntries: [initialPath] }),
  });

  const view = render(
    <ThemeProvider
      theme={theme}
      defaultMode={DEFAULT_COLOR_SCHEME}
    >
      <RouterProvider router={router} />
    </ThemeProvider>,
  );

  return { ...view, router };
}
