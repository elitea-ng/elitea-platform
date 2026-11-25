/**
 * The People and access and Platform pages of R3.6 (issue 2537) and the
 * Access page of R3.9a (issue 2540), mounted through the REAL admin router:
 * each route renders its page, the old Configuration sections lead to the
 * pages their editors moved to, and Access lists the role definitions.
 */
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider } from '@tanstack/react-router';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';
import { testTimeout } from '@/test/timeouts';

import { createAdminRouter } from '../router';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const GO_TESTDATA = join(process.cwd(), '../../services/elitea-main/internal/api/v2/adminaccess/testdata');

function golden(path: string): Record<string, unknown> {
  return JSON.parse(readFileSync(join(GO_TESTDATA, path), 'utf8')) as Record<string, unknown>;
}

interface AdminUiConfigWindow {
  admin_ui_config?: { permissions?: readonly string[]; user_name?: string };
}

function setPermissions(permissions: readonly string[] | undefined): void {
  const config = permissions === undefined ? { user_name: 'ops@example.com' } : { permissions, user_name: 'ops@example.com' };
  (window as unknown as AdminUiConfigWindow).admin_ui_config = config;
}

async function mountAdmin(path: string): Promise<void> {
  window.history.replaceState(null, '', `/admin/app${path}`);
  const router = createAdminRouter();
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
  await screen.findByTestId('admin-nav');
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
  setPermissions(undefined);
  server.use(
    http.get('*/admin/console', () => HttpResponse.json(golden('console/super_admin.golden.json'))),
    http.get('*/admin/access/role-definitions', () =>
      HttpResponse.json(golden('role-definitions/builtin-roles.golden.json')),
    ),
  );
});

afterEach(() => {
  resetGeneratedClient();
  delete (window as unknown as AdminUiConfigWindow).admin_ui_config;
});

describe('the pages of People and access and Platform', () => {
  const PAGES = [
    ['/access', 'admin-access-page', 'access'],
    ['/identity-providers', 'admin-identity-providers-page', 'identity-providers'],
    ['/scim', 'admin-scim-page', 'scim'],
    ['/client-applications', 'admin-client-applications-page', 'client-applications'],
  ] as const;

  for (const [path, page, item] of PAGES) {
    it(`renders ${path} as its own page, with its item active`, async () => {
      await mountAdmin(path);
      expect(await screen.findByTestId(page)).toBeInTheDocument();
      expect(screen.getByTestId(`admin-nav-item-${item}`)).toHaveAttribute('aria-current', 'page');
    }, testTimeout(30_000));
  }

  it('offers both client registries as tabs of Client applications', async () => {
    const user = userEvent.setup();
    await mountAdmin('/client-applications');
    await screen.findByTestId('admin-client-applications-page');
    expect(screen.getByTestId('admin-client-applications-tab-native')).toHaveAttribute('aria-selected', 'true');
    await user.click(screen.getByTestId('admin-client-applications-tab-microsoft'));
    expect(screen.getByTestId('admin-client-applications-tab-microsoft')).toHaveAttribute('aria-selected', 'true');
  }, testTimeout(30_000));
});

describe('a holder of identity.sso.settings.manage alone', () => {
  beforeEach(() => {
    server.use(http.get('*/admin/console', () => HttpResponse.json(golden('console/identity_settings.golden.json'))));
  });

  it('reaches Identity providers and SCIM, and no other page of People and access', async () => {
    await mountAdmin('/identity-providers');
    const area = await screen.findByTestId('admin-nav-area-people_access');
    const items = within(area)
      .getAllByTestId(/^admin-nav-item-/)
      .map((link) => link.getAttribute('data-testid'));
    expect(items).toEqual(['admin-nav-item-identity-providers', 'admin-nav-item-scim']);
    expect(screen.queryAllByTestId(/^admin-nav-area-/)).toHaveLength(1);
    expect(await screen.findByTestId('admin-identity-providers-page')).toBeInTheDocument();
  }, testTimeout(30_000));

  it('is refused Access by its address', async () => {
    await mountAdmin('/access');
    expect(await screen.findByTestId('admin-item-denied')).toHaveAttribute('data-item-id', 'access');
  }, testTimeout(30_000));
});

describe('the Configuration sections whose editors moved', () => {
  function serveSections(): void {
    server.use(
      http.get('*/admin/plugin_config_schemas/administration', () =>
        HttpResponse.json({
          sections: [
            {
              id: 'auth',
              title: 'Authentication',
              managed_surface: 'identity_providers',
              unavailable_reason: 'managed elsewhere',
              fields: [],
            },
            {
              id: 'native_clients',
              title: 'Native clients',
              managed_surface: 'native_clients',
              unavailable_reason: 'managed elsewhere',
              fields: [],
            },
          ],
        }),
      ),
    );
  }

  it('points a moved section at its page instead of rendering the editor', async () => {
    serveSections();
    await mountAdmin('/configuration');

    const moved = await screen.findByTestId('admin-configuration-moved');
    expect(moved).toHaveTextContent('Identity providers');
    expect(screen.queryByTestId('admin-identity-providers-add')).toBeNull();
    expect(screen.queryByText('Not available here')).toBeNull();
  }, testTimeout(30_000));

  it('opens the page from the card and from the section row, the old anchors', async () => {
    const user = userEvent.setup();
    serveSections();
    await mountAdmin('/configuration');

    await user.click(await screen.findByTestId('admin-configuration-moved-link'));
    await vi.waitFor(() => expect(window.location.pathname).toBe('/admin/app/identity-providers'));
    expect(await screen.findByTestId('admin-identity-providers-page')).toBeInTheDocument();

    await user.click(screen.getByTestId('admin-nav-item-configuration'));
    const sections = await screen.findByRole('navigation', { name: 'Configuration sections' });
    await user.click(await within(sections).findByText('Native clients'));
    await vi.waitFor(() => expect(window.location.pathname).toBe('/admin/app/client-applications'));
  }, testTimeout(30_000));
});

describe('Access', () => {
  it('lists every role with its status, the areas it opens, and who may assign it', async () => {
    await mountAdmin('/access');

    const admin = await screen.findByTestId('admin-access-role-admin');
    expect(within(admin).getByText('Platform administrator')).toBeInTheDocument();
    expect(screen.getByTestId('admin-access-item-admin-users')).toHaveAttribute('data-level', 'write');
    expect(screen.getByTestId('admin-access-item-security_auditor-access')).toHaveAttribute('data-level', 'read');
    // The golden's caller holds admin: it may give admin to another person,
    // never super_admin, and never the locked system role.
    expect(screen.getByTestId('admin-access-assignable-admin')).toHaveAttribute('data-assignable', 'true');
    expect(screen.getByTestId('admin-access-assignable-super_admin')).toHaveAttribute('data-assignable', 'false');
    expect(screen.getByTestId('admin-access-assignable-system')).toHaveAttribute('data-assignable', 'false');
  }, testTimeout(30_000));

  it('reaches the permission matrix only for a holder of the matrix', async () => {
    setPermissions(['access.roles.view']);
    await mountAdmin('/access');
    await screen.findByTestId('admin-access-role-admin');
    expect(screen.queryByTestId('admin-access-matrix-link')).toBeNull();
  }, testTimeout(30_000));

  it('links a matrix holder to the permission matrix', async () => {
    const user = userEvent.setup();
    setPermissions(['access.roles.view', 'configuration.roles.permissions.view']);
    await mountAdmin('/access');
    await user.click(await screen.findByTestId('admin-access-matrix-link'));
    await vi.waitFor(() => expect(window.location.pathname).toBe('/admin/app/roles'));
  }, testTimeout(30_000));

  it('is view only for the security auditor, whose document opens Access at read', async () => {
    server.use(http.get('*/admin/console', () => HttpResponse.json(golden('console/security_auditor.golden.json'))));
    await mountAdmin('/access');
    expect(await screen.findByTestId('admin-view-only')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId('admin-access-role-admin')).toBeInTheDocument());
  }, testTimeout(30_000));

  it('says so when the role definitions cannot be read', async () => {
    server.use(
      http.get('*/admin/access/role-definitions', () => HttpResponse.json({ error: 'down' }, { status: 503 })),
    );
    await mountAdmin('/access');
    expect(await screen.findByTestId('admin-access-error')).toBeInTheDocument();
  }, testTimeout(30_000));
});
