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
  admin_ui_config?: { permissions?: readonly string[] };
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

  /*
   * TEMPORARY (remove in R3.6, issue 2537): the Roles link. The document
   * leaves the unavailable Access item out, so the link shows when one of the
   * caller's roles is among the roles the server says open the item AND the
   * caller holds a matrix permission: today's Roles page is the matrix, which
   * access.roles.view does not read (issue 3042).
   */
  describe('the temporary Roles link', () => {
    /** Counts the item reads the server answered, so a test can wait past the answer. */
    let itemReads = 0;

    async function settled(): Promise<void> {
      await waitFor(() => expect(itemReads).toBeGreaterThan(0));
      // One more turn for the answer to reach the navigation.
      await new Promise((resolve) => setTimeout(resolve, 0));
    }

    function serveRolesCase(callerRole: string, itemStatus = 200, builtIn = true, holdsMatrix = true): void {
      itemReads = 0;
      (window as unknown as AdminUiConfigWindow).admin_ui_config = {
        permissions: holdsMatrix ? ['admin.auth.users', 'configuration.roles.permissions.view'] : ['admin.auth.users'],
      };
      server.use(
        getGetAdminConsoleDocumentMockHandler({
          areas: [
            {
              id: 'people_access',
              label_key: 'pages.admin.console.area.people_access',
              items: [{ id: 'users', label_key: 'pages.admin.console.item.users', level: 'read' }],
            },
          ],
          roles: [{ name: callerRole, built_in: builtIn }],
          elevation: null,
          admin_console_areas: true,
        }),
        http.get('*/admin/console/items/access', () => {
          itemReads += 1;
          if (itemStatus !== 200) return new HttpResponse(null, { status: itemStatus });
          return HttpResponse.json({
            id: 'access',
            area: 'people_access',
            label_key: 'pages.admin.console.item.access',
            area_label_key: 'pages.admin.console.area.people_access',
            available: false,
            granting_roles: [
              { name: 'admin', built_in: true, level: 'write' },
              { name: 'security_auditor', built_in: true, level: 'read' },
            ],
          });
        }),
      );
    }

    it('shows Roles under People and access for a caller whose role opens it and who reads the matrix', async () => {
      serveRolesCase('admin');
      await mountLayout();

      const link = await screen.findByTestId('admin-nav-item-access');
      expect(link).toHaveTextContent('Roles');
      expect(link).toHaveAttribute('href', '/roles');
      expect(screen.getByTestId('admin-nav-area-people_access')).toContainElement(link);
    });

    it('leaves Roles out for the security auditor, whose access.roles.view does not read the matrix', async () => {
      serveRolesCase('security_auditor', 200, true, false);
      await mountLayout();

      await screen.findByTestId('admin-nav-item-users');
      await settled();
      expect(screen.queryByTestId('admin-nav-item-access')).not.toBeInTheDocument();
    });

    it('leaves Roles out for a caller none of whose roles opens it', async () => {
      serveRolesCase('llm_manager');
      await mountLayout();

      await screen.findByTestId('admin-nav-item-users');
      await settled();
      expect(screen.queryByTestId('admin-nav-item-access')).not.toBeInTheDocument();
    });

    it('leaves Roles out for a custom role that only shares a built-in role name', async () => {
      serveRolesCase('admin', 200, false);
      await mountLayout();

      await screen.findByTestId('admin-nav-item-users');
      await settled();
      expect(screen.queryByTestId('admin-nav-item-access')).not.toBeInTheDocument();
    });

    it('leaves Roles out when the roles that open it cannot be read', async () => {
      serveRolesCase('admin', 500);
      await mountLayout();

      await screen.findByTestId('admin-nav-item-users');
      await settled();
      expect(screen.queryByTestId('admin-nav-item-access')).not.toBeInTheDocument();
    });
  });

  it('renders the two unlabelled groups when the switch is off', async () => {
    server.use(getGetAdminConsoleDocumentMockHandler({ ...getGetAdminConsoleDocumentResponseMock(), admin_console_areas: false }));
    await mountLayout();

    expect(await screen.findByTestId('admin-nav')).toBeInTheDocument();
    expect(screen.getByTestId('admin-nav-item-users')).toBeInTheDocument();
    expect(screen.queryAllByTestId(/^admin-nav-area-/)).toHaveLength(0);
  });
});
