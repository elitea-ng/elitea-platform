/**
 * The index route of the console through the REAL admin router (ADR-0059
 * decision 6; R3.3a, issue 2533): a caller whose document opens exactly one
 * area lands on that area's first item instead of Home, and with the
 * `admin_console_areas` switch off the index keeps rendering Users.
 */
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider } from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';
import { testTimeout } from '@/test/timeouts';

import { createAdminRouter } from '../router';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const GOLDEN = join(process.cwd(), '../../services/elitea-main/internal/api/v2/adminaccess/testdata/console');

function golden(role: string): Record<string, unknown> {
  return JSON.parse(readFileSync(join(GOLDEN, `${role}.golden.json`), 'utf8')) as Record<string, unknown>;
}

function mountAdmin(path: string): ReturnType<typeof createAdminRouter> {
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
  return router;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
  // Whatever page the index lands on reads its own rows; none are needed here.
  server.use(http.get('*/admin/*', () => HttpResponse.json({ rows: [], total: 0, items: [] })));
});

afterEach(() => {
  resetGeneratedClient();
  window.localStorage.clear();
});

describe('the index route', () => {
  it('lands a caller with one area on its first item, not on Home', async () => {
    // identity_settings opens People and access only: Identity providers and SCIM.
    server.use(http.get('*/admin/console', () => HttpResponse.json(golden('identity_settings'))));
    const router = mountAdmin('/');
    await vi.waitFor(() => expect(router.state.location.pathname).toBe('/identity-providers'), {
      timeout: 10_000,
    });
    expect(await screen.findByTestId('admin-nav-item-identity-providers')).toHaveAttribute('aria-current', 'page');
    expect(screen.queryByTestId('admin-home')).not.toBeInTheDocument();
  }, testTimeout(30_000));

  it('stays on Home for a caller with more than one area', async () => {
    server.use(http.get('*/admin/console', () => HttpResponse.json(golden('security_auditor'))));
    const router = mountAdmin('/');
    expect(await screen.findByTestId('admin-home', undefined, { timeout: 10_000 })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/');
    // Home is no item's page: nothing in the navigation is current.
    expect(screen.getByTestId('admin-nav').querySelector('[aria-current="page"]')).toBeNull();
  }, testTimeout(30_000));

  it('keeps rendering Users while the six-area switch is off', async () => {
    server.use(
      http.get('*/admin/console', () => HttpResponse.json({ ...golden('super_admin'), admin_console_areas: false })),
    );
    mountAdmin('/');
    expect(await screen.findByTestId('admin-nav', undefined, { timeout: 10_000 })).toBeInTheDocument();
    expect(await screen.findByRole('heading', { level: 1, name: 'Users' }, { timeout: 10_000 })).toBeInTheDocument();
    expect(screen.queryByTestId('admin-home')).not.toBeInTheDocument();
  }, testTimeout(30_000));
});
