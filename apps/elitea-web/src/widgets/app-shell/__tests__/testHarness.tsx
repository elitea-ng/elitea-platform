import type { ReactElement, ReactNode } from 'react';

import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Link,
  Outlet,
  RouterProvider,
} from '@tanstack/react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { render } from '@testing-library/react';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { t } from '@/shared/i18n';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/**
 * Router + QueryClient + theme harness for widgets that use `useBlocker`
 * (which needs somewhere ELSE to navigate to in order to observe blocking)
 * and TanStack Query's `useQueryClient`. See
 * `widgets/create-button/__tests__/testRouter.tsx` for the base-case
 * rationale; this adds a second sibling route and a real `<Link>` so tests
 * can drive an actual navigation attempt through the router, which is what
 * `useBlocker` intercepts.
 *
 * The second route's path is a REAL registered pattern (`/help-center`,
 * R1's `_shell/help-center.tsx`), not an invented one: `router.tsx`'s
 * `declare module '@tanstack/react-router' { interface Register … }`
 * ambient augmentation makes every `<Link to>`/`useNavigate` call in the
 * WHOLE program — this local test router's own JSX included — type-check
 * against the real app-wide route tree, regardless of what this file's own
 * `routeTree` actually contains at runtime.
 *
 * `options.initialPath` (default `/`) lets a test mount `ui` at an
 * arbitrary starting pathname — e.g. `/onboarding` (also a REAL registered
 * pattern, `routes/_shell/onboarding.tsx`) for AppShell's onboarding-page
 * chrome regression coverage — without needing a THIRD hard-coded route:
 * the "main" route's own `path` is just that string instead of the literal
 * `'/'`. `createRoute`'s `path` accepts a plain runtime `string` here (not
 * a literal-typed one) for the same reason this function has no explicit
 * return-type annotation below.
 *
 * The returned `queryClient` (the exact instance passed to
 * `QueryClientProvider`) lets a test seed/inspect cache state directly —
 * e.g. NavBlockerDialog's R3 regression coverage, which needs to prove a
 * confirmed navigation does NOT wipe it.
 */
// No explicit return-type annotation (deliberate): `ReturnType<typeof
// createRouter>` resolves against the generic's DEFAULT type arguments,
// which do not structurally match THIS call's specific route-tree
// instantiation under `exactOptionalPropertyTypes` — letting TS infer the
// precise type from the function body avoids that mismatch.
export async function renderWithNavigation(
  ui: ReactNode,
  options?: { initialPath?: string; queryClient?: QueryClient },
) {
  const initialPath = options?.initialPath ?? '/';
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const homeRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: initialPath,
    component: () => (
      <>
        {ui as ReactElement}
        <Link to="/help-center">{t('widgets.appShell.test.goElsewhere', 'go elsewhere')}</Link>
      </>
    ),
  });
  const elsewhereRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/help-center',
    component: () => <div>{t('widgets.appShell.test.elsewhere', 'elsewhere')}</div>,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([homeRoute, elsewhereRoute]),
    history: createMemoryHistory({ initialEntries: [initialPath] }),
  });
  await router.load();

  /*
   * `options.queryClient` lets a test hand in a client whose cache is ALREADY
   * populated. That is the only way to reproduce a shell that mounts with both
   * of its lists already answered — a remount, not a cold page load — which is
   * the exact condition under which AppShell's auto-select effect and the
   * hydration effect run in the same passive-effect flush. A client created
   * here is empty by construction, so a test using it can only ever exercise
   * the cold path.
   */
  const queryClient =
    options?.queryClient ?? new QueryClient({ defaultOptions: { queries: { retry: false } } });

  const result = render(
    <ThemeProvider
      theme={theme}
      defaultMode={DEFAULT_COLOR_SCHEME}
    >
      <CssBaseline />
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </ThemeProvider>,
  );
  return { ...result, router, queryClient };
}
