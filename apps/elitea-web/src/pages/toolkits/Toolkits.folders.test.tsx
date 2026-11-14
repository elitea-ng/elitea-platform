import type { ReactElement } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getGetPlatformSettingsMockHandler, getGetPlatformSettingsResponseMock } from '@/shared/api/generated/admin/admin.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getListToolkitInstancesMockHandler } from '@/shared/api/generated/toolkits/toolkits.msw';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { Toolkits } from './Toolkits';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const FOLDERS = '/api/v2/elitea_core/entity_folders/prompt_lib/project-1/toolkit';

function row(id: string, name: string) {
  return { id, type: 'github', name, description: '', settings: {}, meta: {}, created_at: '2026-01-01T00:00:00Z', author_id: 1 };
}

function renderAt(path: string, content: ReactElement) {
  const root = createRootRoute({ component: () => <Outlet /> });
  const tab = createRoute({
    getParentRoute: () => root,
    path: '/toolkits/$tab',
    validateSearch: (s: Record<string, unknown>) => ({ ...(s['entity_folder'] === undefined ? {} : { entity_folder: String(s['entity_folder']) }) }),
    component: () => content,
  });
  const create = createRoute({ getParentRoute: () => root, path: '/toolkits/create' });
  const router = createRouter({
    routeTree: root.addChildren([tab, create]),
    history: createMemoryHistory({ initialEntries: [path] }),
    context: { auth: { getSelectedProjectId: () => 'project-1' } },
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } } });
  return {
    router,
    ...render(
      <QueryClientProvider client={client}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <RouterProvider router={router} />
        </ThemeProvider>
      </QueryClientProvider>,
    ),
  };
}

function folder(id: string, ids: number[]) {
  return { id, kind: 'toolkit', name: `F${id}`, position: 1, item_ids: ids, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    getGetPlatformSettingsMockHandler(getGetPlatformSettingsResponseMock({ mcp_enabled: true, mcp_in_menu_enabled: true })),
    getListToolkitInstancesMockHandler({ rows: [row('11', 'Alpha'), row('12', 'Beta')], total: 2 } as never),
  );
});
afterEach(() => {
  resetGeneratedClient();
});

describe('Toolkits with a folder open', () => {
  it('counts and lists only the folder, and an empty folder says so instead of offering to create a toolkit', async () => {
    server.use(http.get(FOLDERS, () => HttpResponse.json({ items: [folder('1', [11]), folder('2', [])] })));
    const { router } = renderAt('/toolkits/all?entity_folder=1', <Toolkits />);
    await waitFor(() => expect(screen.getAllByTestId('toolkit-card')).toHaveLength(1));
    expect(screen.getByText('Alpha')).toBeInTheDocument();
    await router.navigate({ to: '/toolkits/$tab', params: { tab: 'all' }, search: { entity_folder: '2' } as never });
    expect(await screen.findByTestId('toolkits-empty-folder')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /create/i })).not.toBeInTheDocument();
  });

  it('shows every toolkit when the folders query fails, instead of a blank list', async () => {
    server.use(http.get(FOLDERS, () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    renderAt('/toolkits/all?entity_folder=1', <Toolkits />);
    await waitFor(() => expect(screen.getAllByTestId('toolkit-card')).toHaveLength(2));
  });

  it('drops a stale folder id and shows everything', async () => {
    server.use(http.get(FOLDERS, () => HttpResponse.json({ items: [folder('1', [11])] })));
    const { router } = renderAt('/toolkits/all?entity_folder=99', <Toolkits />);
    await waitFor(() => expect(screen.getAllByTestId('toolkit-card')).toHaveLength(2));
    await waitFor(() => expect(router.state.location.search).not.toHaveProperty('entity_folder', '99'));
  });
});
