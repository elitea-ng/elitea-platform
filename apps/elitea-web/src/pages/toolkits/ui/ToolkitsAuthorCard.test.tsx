/*
 * `ToolkitsAuthorCard` — the right-rail slot the toolkits and MCP list pages
 * fill.
 *
 * It had no test, and its whole body is two decisions:
 *
 *  1. it renders NOTHING unless the viewer is looking at their own personal
 *     project. A component that always rendered would show a stranger's author
 *     card on a shared project's toolkit list;
 *  2. the pathname it hands the statistic lookup is `/mcps` on an MCP screen
 *     and `/toolkits` otherwise. `/mcps` matches no entry in the statistic map,
 *     so the card there shows the author and no count — the same nothing the
 *     reference deployment renders.
 *
 * The viewer comes from the ROUTER CONTEXT, so each case installs a different
 * context rather than replacing the hook: mocks stop at the network boundary
 * here (R-M1 forbids `vi.mock`).
 */
import { ThemeProvider } from '@mui/material/styles';
import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { createTestQueryClient } from '@/features/toolkits/__tests__/testUtils';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { ToolkitsAuthorCard } from './ToolkitsAuthorCard';

interface Viewer {
  readonly id?: string;
  readonly personal_project_id?: string;
}

const AUTHOR = {
  id: 7,
  name: 'Autotest Author',
  avatar: null,
  statistic: { total_toolkits: 3, public_toolkits: 1 },
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  // Every author read the card can issue. Registered unconditionally so a case
  // that renders nothing and a case that renders the card are told apart by
  // the DOM, not by which request happened to be handled.
  server.use(
    http.get(/\/api\/v2\/.*author.*/, () => HttpResponse.json(AUTHOR)),
    http.get(/\/api\/v2\/.*/, () => HttpResponse.json({})),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

function renderAuthorCard(viewer: Viewer | undefined, props: { projectId?: string; isMCP: boolean }) {
  const rootRoute = createRootRoute({
    component: () => (
      <ToolkitsAuthorCard
        projectId={props.projectId}
        isMCP={props.isMCP}
      />
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getUser: () => viewer } },
  });
  return render(
    <QueryClientProvider client={createTestQueryClient()}>
      <ThemeProvider
        theme={buildEliteaTheme(DEFAULT_BRAND_PACK)}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

describe('ToolkitsAuthorCard', () => {
  it('renders nothing while the viewer is in someone else\'s project', async () => {
    const { container } = renderAuthorCard(
      { id: '7', personal_project_id: '42' },
      { projectId: '1', isMCP: false },
    );
    await waitFor(() => expect(container).toBeInTheDocument());
    expect(container.textContent).toBe('');
  });

  it('renders the card in the viewer\'s own personal project', async () => {
    const { container } = renderAuthorCard(
      { id: '7', personal_project_id: '42' },
      { projectId: '42', isMCP: false },
    );
    // Either the loaded card or its loading skeleton — both are "the card
    // mounted"; asserting the loaded name would make this a test of the author
    // endpoint's shape, which is not this component's decision.
    await waitFor(() => expect(container.querySelector('[data-testid^="entity-rail-author"]')).not.toBeNull());
  });

  it('renders the card when the viewer has no personal project recorded', async () => {
    // shouldShowAuthorCard's third branch: with nothing to compare, the card is
    // shown. A component that dropped it would hide the card for every viewer
    // whose session predates personal-project provisioning.
    const { container } = renderAuthorCard({ id: '7' }, { projectId: '1', isMCP: false });
    await waitFor(() => expect(container.querySelector('[data-testid^="entity-rail-author"]')).not.toBeNull());
  });

  it('renders on the MCP screen too, under its own pathname', async () => {
    const { container } = renderAuthorCard(
      { id: '7', personal_project_id: '42' },
      { projectId: '42', isMCP: true },
    );
    await waitFor(() => expect(container.querySelector('[data-testid^="entity-rail-author"]')).not.toBeNull());
    // `/mcps` matches no entry in the statistic map, so no toolkit count row is
    // drawn — the difference between the two pathnames this component chooses.
    expect(container.querySelector('[data-testid="entity-rail-author-stat-total"]')).toBeNull();
  });
});
