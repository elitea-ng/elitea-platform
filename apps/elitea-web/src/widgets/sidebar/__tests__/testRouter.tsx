import type { ReactElement, ReactNode } from 'react';

import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router';
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, type RenderResult } from '@testing-library/react';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/**
 * Self-contained router harness for this widget's component tests: the
 * sidebar's links and hooks read the router, so every test renders inside a
 * real memory-history router.
 *
 * `QueryClientProvider` (a fresh `QueryClient` per call, never shared
 * across tests — same isolation `NotificationListItem.test.tsx` uses) was
 * added alongside `NotificationButton` (SHELL-013): `SidebarBody` now
 * mounts a component that calls `useNotificationsList` (`@tanstack/
 * react-query`'s `useQuery`), which throws "No QueryClient set" without a
 * provider above it — every existing test in this directory (`Sidebar.
 * test.tsx`, `ProjectSwitcher.test.tsx`) renders through this same helper
 * and would otherwise start failing the moment `SidebarBody` grew that
 * child, regardless of whether the individual test ever opens the
 * notification popover.
 */
export async function renderAtPath(pathname: string, ui: ReactNode): Promise<RenderResult> {
  const rootRoute = createRootRoute();
  const testRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: pathname.replace(/^\//, ''),
    component: () => ui as ReactElement,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([testRoute]),
    history: createMemoryHistory({ initialEntries: [pathname] }),
  });
  await router.load();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
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
}
