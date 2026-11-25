/**
 * "Tasks" and "Schedules & Tasks" became one console item, "Schedules and
 * tasks" (ADR-0059 decision 6, R3.2, issue 2532). Both paths keep working:
 * each resolves to its own page through the REAL admin router, and the one
 * item is active on both.
 */
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider } from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  getGetAdminConsoleDocumentMockHandler,
  getGetAdminConsoleDocumentResponseMock,
} from '@/shared/api/generated/admin/admin.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';
import { testTimeout } from '@/test/timeouts';

import { installWebStorageShim } from '../../../test/webstorage';
import { createAdminRouter } from '../router';

installWebStorageShim();

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

async function mountAt(path: string): Promise<ReturnType<typeof createAdminRouter>> {
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
  await screen.findByTestId('admin-nav-area-platform');
  return router;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
  // The contract's example document: a global administrator, areas on.
  server.use(getGetAdminConsoleDocumentMockHandler({ ...getGetAdminConsoleDocumentResponseMock(), admin_console_areas: true }));
});

afterEach(() => {
  resetGeneratedClient();
  window.localStorage.clear();
});

describe('/schedules and /tasks under one item', () => {
  for (const [path, page] of [
    ['/schedules', 'admin-schedules-tasks-tab-schedules'],
    ['/tasks', 'admin-schedules-tasks-tab-tasks'],
  ] as const) {
    it(`resolves ${path} to its own page and marks "Schedules and tasks" active`, async () => {
      const router = await mountAt(path);
      expect(router.state.matches.map((match) => match.routeId)).toEqual(['__root__', path]);

      const item = screen.getByTestId('admin-nav-item-schedules-tasks');
      expect(item).toHaveAttribute('aria-current', 'page');
      expect(screen.getAllByRole('link', { current: 'page' })).toEqual([item]);
      expect(await screen.findByTestId(page)).toHaveAttribute('aria-selected', 'true');
    }, testTimeout(30_000));
  }
});
