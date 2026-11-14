import type { ReactElement } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Outlet, RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter, type AnyRouter } from '@tanstack/react-router';
import { render, type RenderResult } from '@testing-library/react';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

export const BASE = '/api/v2';
export const PROJECT = '7';

export interface FolderFixture {
  readonly id: string;
  readonly name: string;
  readonly position: number;
  readonly item_ids: readonly number[];
}

export function wireFolder(kind: string, fixture: FolderFixture): Record<string, unknown> {
  return { ...fixture, kind, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' };
}

/** The folder list of one kind; every call is recorded in `requests`. */
export function serveFolders(kind: string, folders: readonly FolderFixture[]): void {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/entity_folders/prompt_lib/${PROJECT}/${kind}`, () =>
      HttpResponse.json({ items: folders.map((folder) => wireFolder(kind, folder)) }),
    ),
  );
}

export interface Rendered extends RenderResult {
  readonly router: AnyRouter;
}

/** Renders `content` on `/skills/all` with a router whose `entity_folder` search param is the app's `text()` (a string). */
export function renderInRouter(content: ReactElement, path = '/skills/all'): Rendered {
  const root = createRootRoute({ component: () => <Outlet /> });
  const route = createRoute({
    getParentRoute: () => root,
    path: '/skills/$tab',
    validateSearch: (search: Record<string, unknown>) => ({
      ...(search['entity_folder'] === undefined ? {} : { entity_folder: String(search['entity_folder']) }),
    }),
    component: () => content,
  });
  const router = createRouter({
    routeTree: root.addChildren([route]),
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider
        theme={theme}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return { ...view, router };
}
