import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getGetAuthorDetailMockHandler, getListApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getListTagsMockHandler } from '@/shared/api/generated/tags/tags.msw';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { server } from '../../../test/setup';
import { AGENTS_TAB_ADMIN_PERMISSION } from '../lib/constants';

import { UserPublicPage } from './UserPublicPage';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/** Same shape as `UserPublicPage.test.tsx`'s own fixture, plus the `id`/`personal_project_id` the rail reads off the router context. */
function renderPage(props: Partial<Parameters<typeof UserPublicPage>[0]> = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  function Page() {
    return (
      <UserPublicPage
        tab="pipelines"
        onTabChange={vi.fn()}
        statuses={[]}
        onStatusesChange={vi.fn()}
        authorId="5"
        authorName="Ada"
        {...props}
      />
    );
  }

  const rootRoute = createRootRoute({
    component: () => (
      <ThemeProvider
        theme={theme}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        <CssBaseline />
        <QueryClientProvider client={queryClient}>
          <Page />
        </QueryClientProvider>
      </ThemeProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: {
      auth: {
        getSelectedProjectId: () => 'proj-1',
        getUser: () => ({ id: '5', personal_project_id: 'proj-1', permissions: [AGENTS_TAB_ADMIN_PERMISSION] }),
      },
    },
  });
  render(<RouterProvider router={router} />);
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    getListApplicationsMockHandler({ rows: [], total: 0, page: 1, page_size: 20, total_pages: 0 }),
    getListTagsMockHandler({ rows: [{ id: 1, name: 'alpha', data: {} }], total: 1 }),
    getGetAuthorDetailMockHandler({ id: 5, name: 'Ada Lovelace', total_pipelines: 4, total_applications: 9, public_applications: 2 }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('UserPublicPage right-hand rail', () => {
  it('renders the rail with the project tag chips', async () => {
    renderPage();

    expect(await screen.findByTestId('entity-rail')).toBeInTheDocument();
    expect(await screen.findByTestId('tags-panel-chip-alpha')).toBeInTheDocument();
  });

  it('keys the author statistic off the TAB, since /user-public matches no route prefix', async () => {
    renderPage({ tab: 'pipelines' });

    expect(await screen.findByTestId('entity-rail-author')).toBeInTheDocument();
    expect(screen.getByTestId('entity-rail-author-total')).toHaveTextContent('Pipelines:4');
    // `/pipelines` declares no publishedKey — no Published row, even though
    // the author detail carries `public_applications`.
    expect(screen.queryByTestId('entity-rail-author-published')).toBeNull();
  });

  it('switches the statistic to Agents on the agents tab', async () => {
    renderPage({ tab: 'agents' });

    expect(await screen.findByTestId('entity-rail-author-total')).toHaveTextContent('Agents:9');
    expect(screen.getByTestId('entity-rail-author-published')).toHaveTextContent('Published:2');
  });

  it('shows the author card, not trending authors, while an author is in URL scope', async () => {
    renderPage();

    expect(await screen.findByTestId('entity-rail-author')).toBeInTheDocument();
    expect(screen.queryByText('Trending Authors')).toBeNull();
  });
});
