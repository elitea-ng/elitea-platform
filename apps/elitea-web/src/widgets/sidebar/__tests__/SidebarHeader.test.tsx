/**
 * The rail's header row: the bell sits beside the logo (not at the bottom of
 * the rail), and the collapse toggle is a button in the same row.
 *
 * The bell reads `useRouteContext`/`useNavigate`, so the header renders
 * inside a router.
 */
import type { ReactElement } from 'react';

import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';

import { buildEliteaTheme, DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME } from '@/shared/brand';

import { SidebarHeader } from '../ui/SidebarHeader';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

async function renderHeader(collapsed: boolean): Promise<void> {
  const auth = { getUser: () => ({ personal_project_id: '1' }), getSelectedProjectId: () => undefined };
  const rootRoute = createRootRoute();
  const homeRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/',
    component: () => (<SidebarHeader collapsed={collapsed} onToggleCollapsed={() => {}} />) as ReactElement,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([homeRoute]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth },
  });
  await router.load();

  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
      <CssBaseline />
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </ThemeProvider>,
  );
}

describe('SidebarHeader — the notification bell sits in the header row', () => {
  it('renders the bell inside the same row as the logo toggle', async () => {
    await renderHeader(false);

    const toggle = screen.getByTestId('sidebar-toggle');
    const bell = screen.getByTestId('sidebar-notification-button');

    // `contains` rather than comparing parents directly, because the bell
    // brings its own wrapper. What matters is that it lives inside the header
    // row — which stops being true the moment it moves back beside the footer.
    const headerRow = toggle.parentElement;
    expect(headerRow).not.toBeNull();
    expect(headerRow?.contains(bell)).toBe(true);
  });

  it('hides the bell while collapsed — the rail is too narrow for two controls', async () => {
    await renderHeader(true);

    expect(screen.getByTestId('sidebar-toggle')).toBeInTheDocument();
    expect(screen.queryByTestId('sidebar-notification-button')).not.toBeInTheDocument();
  });

  it('carries the collapse toggle in the header row, named for what it does', async () => {
    await renderHeader(false);
    const toggle = screen.getByTestId('sidebar-collapse-toggle');
    expect(toggle).toHaveAccessibleName('Collapse sidebar');
    expect(screen.getByTestId('sidebar-toggle').parentElement?.contains(toggle)).toBe(true);
  });
});
