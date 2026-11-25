/**
 * The admin layout over the console capability document (ADR-0059 R3.1b,
 * issue 2611).
 *
 * The document carries the `admin_console_areas` switch (R3.2, issue 2532),
 * so until it answers the navigation is unknown: a pending read shows a
 * loading state and a failed read shows an error with a retry control and no
 * navigation item, never an empty navigation, which would read as "no
 * access". Once it answers, the switch picks the six labelled areas or the
 * two unlabelled groups.
 */
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  getGetAdminConsoleDocumentMockHandler,
  getGetAdminConsoleDocumentResponseMock,
} from '@/shared/api/generated/admin/admin.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { installWebStorageShim } from '../../test/webstorage';
import { AdminLayout } from './AdminLayout';

installWebStorageShim();

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

interface AdminUiConfigWindow {
  admin_ui_config?: { permissions?: readonly string[]; access?: string };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
  (window as unknown as AdminUiConfigWindow).admin_ui_config = { permissions: ['admin.auth.users'] };
});

afterEach(() => {
  resetGeneratedClient();
  window.localStorage.clear();
  delete (window as unknown as AdminUiConfigWindow).admin_ui_config;
});

async function mountLayout(): Promise<void> {
  const rootRoute = createRootRoute({ component: () => <AdminLayout /> });
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/',
    component: () => <p data-testid="admin-layout-page">page</p>,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  await screen.findByTestId('admin-layout-page');
}

function failConsoleRead(): void {
  server.use(http.get('*/admin/console', () => new HttpResponse(null, { status: 503 })));
}

describe('AdminLayout with the document-driven navigation', () => {
  it('shows a loading state while the document is pending', async () => {
    server.use(getGetAdminConsoleDocumentMockHandler(() => new Promise(() => {})));
    await mountLayout();

    expect(screen.getByTestId('admin-console-loading')).toBeInTheDocument();
    expect(screen.queryByTestId('admin-nav')).not.toBeInTheDocument();
    expect(screen.queryByTestId('admin-console-error')).not.toBeInTheDocument();
  });

  it('shows an error with a retry control and no navigation item when the read fails', async () => {
    failConsoleRead();
    await mountLayout();

    expect(await screen.findByTestId('admin-console-error')).toBeInTheDocument();
    expect(screen.getByTestId('admin-console-retry')).toBeInTheDocument();
    expect(screen.queryByTestId('admin-nav')).not.toBeInTheDocument();
    expect(screen.queryAllByTestId(/^admin-nav-item-/)).toHaveLength(0);
  });

  it('reads the document again from the retry control and leaves the error state when it answers', async () => {
    failConsoleRead();
    await mountLayout();
    await screen.findByTestId('admin-console-error');

    server.use(getGetAdminConsoleDocumentMockHandler(getGetAdminConsoleDocumentResponseMock()));
    await userEvent.click(screen.getByTestId('admin-console-retry'));

    await waitFor(() => expect(screen.queryByTestId('admin-console-error')).not.toBeInTheDocument());
    expect(screen.queryByTestId('admin-console-loading')).not.toBeInTheDocument();
  });
});

describe('AdminLayout when the server refuses the document itself', () => {
  it('says so, offers no retry and shows no navigation item', async () => {
    server.use(http.get('*/admin/console', () => new HttpResponse(null, { status: 403 })));
    await mountLayout();

    expect(await screen.findByTestId('admin-console-refused')).toBeInTheDocument();
    expect(screen.queryByTestId('admin-console-retry')).not.toBeInTheDocument();
    expect(screen.queryAllByTestId(/^admin-nav-item-/)).toHaveLength(0);
  });
});

describe('AdminLayout once the document answers', () => {
  it('renders the six labelled areas when the switch is on', async () => {
    server.use(getGetAdminConsoleDocumentMockHandler({ ...getGetAdminConsoleDocumentResponseMock(), admin_console_areas: true }));
    await mountLayout();

    expect(await screen.findByTestId('admin-nav-area-people_access')).toBeInTheDocument();
    expect(screen.getByTestId('admin-nav-item-users')).toBeInTheDocument();
  });

  it('renders the two unlabelled groups when the switch is off', async () => {
    server.use(getGetAdminConsoleDocumentMockHandler({ ...getGetAdminConsoleDocumentResponseMock(), admin_console_areas: false }));
    await mountLayout();

    expect(await screen.findByTestId('admin-nav')).toBeInTheDocument();
    expect(screen.getByTestId('admin-nav-item-users')).toBeInTheDocument();
    expect(screen.queryAllByTestId(/^admin-nav-area-/)).toHaveLength(0);
  });
});
