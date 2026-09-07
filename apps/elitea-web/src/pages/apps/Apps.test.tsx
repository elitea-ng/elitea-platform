import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { getListApplicationsMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { getListToolkitsMockHandler } from '@/shared/api/generated/toolkits/toolkits.msw';
import { getModerationStatusMockHandler } from '@/shared/api/generated/admin/admin.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderAppsRoute } from './__tests__/testRouter';

function applicationsList(total: number) {
  return {
    rows: Array.from({ length: total }, (_, index) => ({
      id: String(index + 1),
      name: `App ${index + 1}`,
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
      owner_id: 'user-1',
      is_forked: false,
      meta: null,
      has_interrupt: false,
      tags: [],
    })),
    total,
    page: 1,
    page_size: 20,
    total_pages: total > 0 ? 1 : 0,
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    getListToolkitsMockHandler({}),
    /*
     * PIN THE MODERATION STATUS. The argument is the whole point of this line.
     *
     * EVERY catalogue render now asks for moderation status: `useModerationRequests`
     * sends one request per catalogue entry, and it only stopped sending them
     * before because the selected project id was undefined and the queries were
     * disabled.
     *
     * The generated handler answers with `getModerationStatusResponseMock()`,
     * whose row carries `status: faker.helpers.arrayElement(['pending',
     * 'approved', 'rejected'])`. A card whose status reads `pending` correctly
     * hides its Configure button, because the user already has a request in
     * flight. So one run in three failed, and calling the generated handler
     * with no argument did not help: the randomness IS the defect.
     *
     * An empty row set is the precondition these cases actually describe — a
     * project with no moderation request against the entity, where Configure
     * is offered. Any test that wants the pending or rejected branch has to
     * ask for it explicitly.
     */
    getModerationStatusMockHandler({ total: 0, rows: [] }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('Apps (ROUTE-036/039)', () => {
  it('redirects a bare /apps to /apps/catalog when the project has no configured applications', async () => {
    server.use(getListApplicationsMockHandler(applicationsList(0)));
    const { router } = renderAppsRoute('/apps/', { projectId: 'proj-1' });

    await waitFor(() => expect(router.state.location.pathname).toBe('/apps/catalog'));
    expect(await screen.findByText('Wikis')).toBeInTheDocument();
  });

  it('a bare /apps redirects to /apps/catalog even when the project HAS configured applications — a genuine, faithfully-reproduced baseline race, not a defect in this port', async () => {
    // `hasApplications` starts `false` (the query has not resolved on the
    // very first render), so `normalizeAppsTab(undefined, false)` computes
    // 'catalog' and the redirect effect fires immediately — to
    // `/apps/catalog`. Once there, 'catalog' is itself a RECOGNISED tab
    // value, so `normalizeAppsTab('catalog', hasApplications)` takes the
    // "already valid, pass through" branch on every later render and never
    // re-derives off the (by-then-true) `hasApplications`, no matter how
    // long the app stays mounted. This is not specific to the port: the
    // baseline's `Apps.jsx` has the exact same `useMemo`/`useEffect` shape
    // over an equally-async `useToolkitsListQuery`, so a real user visiting
    // bare `/apps` lands on the catalog tab first every time in practice,
    // regardless of whether the project has applications — reproduced
    // byte-for-byte (N4), not fixed.
    server.use(getListApplicationsMockHandler(applicationsList(1)));
    const { router } = renderAppsRoute('/apps/', { projectId: 'proj-1' });

    await waitFor(() => expect(router.state.location.pathname).toBe('/apps/catalog'));
  });

  it('redirects the legacy /apps/all alias to /apps/catalog', async () => {
    server.use(getListApplicationsMockHandler(applicationsList(0)));
    const { router } = renderAppsRoute('/apps/all', { projectId: 'proj-1' });

    await waitFor(() => expect(router.state.location.pathname).toBe('/apps/catalog'));
  });

  it('redirects an unrecognised :tab value to the computed default', async () => {
    server.use(getListApplicationsMockHandler(applicationsList(0)));
    const { router } = renderAppsRoute('/apps/bogus', { projectId: 'proj-1' });

    await waitFor(() => expect(router.state.location.pathname).toBe('/apps/catalog'));
  });

  it('renders the App Catalog tab content at /apps/catalog', async () => {
    server.use(getListApplicationsMockHandler(applicationsList(0)));
    renderAppsRoute('/apps/catalog', { projectId: 'proj-1' });

    expect(await screen.findByText('Wikis')).toBeInTheDocument();
    expect(screen.getByText('Inventory')).toBeInTheDocument();
  });

  // DEFECT: `searchForAppsTab` only returned the same object reference when
  // `view` was absent. The route schema prefaults `view` to `'grid'`, so the
  // key is always present. Every Catalog-tab mount therefore produced a
  // new object. `Apps.tsx`'s reference-equality guard then fired a replace
  // navigation with `view` stripped. `validateSearch` put `view=grid`
  // straight back into the URL. Evidence: the URL grew a `?view=grid` query
  // that the user never asked for, one history replace per mount.
  it('issues no redirect on a Catalog-tab mount when `view` is already the default', async () => {
    server.use(getListApplicationsMockHandler(applicationsList(0)));
    const navigations: unknown[] = [];
    const { router } = renderAppsRoute('/apps/catalog', {
      projectId: 'proj-1',
      onRouterCreated: (created) => {
        const navigate = created.navigate.bind(created);
        created.navigate = ((options: never) => {
          navigations.push(options);
          return navigate(options);
        }) as typeof created.navigate;
      },
    });

    expect(await screen.findByText('Wikis')).toBeInTheDocument();
    await waitFor(() => expect(router.state.location.pathname).toBe('/apps/catalog'));
    expect(navigations).toEqual([]);
  });

  // The reset itself must survive: a non-default `view` carried onto the
  // Catalog tab is still normalised back to the default.
  it('resets a non-default `view` on the Catalog tab', async () => {
    server.use(getListApplicationsMockHandler(applicationsList(0)));
    const { router } = renderAppsRoute('/apps/catalog?view=list', { projectId: 'proj-1' });

    await waitFor(() => expect(router.state.location.searchStr).not.toContain('list'));
    expect(router.state.location.pathname).toBe('/apps/catalog');
  });

  it('renders the (composition-gap) Applications tab panel at /apps/applications without crashing', async () => {
    server.use(getListApplicationsMockHandler(applicationsList(1)));
    renderAppsRoute('/apps/applications', { projectId: 'proj-1' });

    await waitFor(() => expect(screen.getByTestId('apps-applications-tab-panel')).toBeInTheDocument());
    expect(screen.queryByText('Wikis')).not.toBeInTheDocument();
  });

  it('switching tabs updates the URL', async () => {
    server.use(getListApplicationsMockHandler(applicationsList(1)));
    const user = userEvent.setup();
    const { router } = renderAppsRoute('/apps/applications', { projectId: 'proj-1' });

    await waitFor(() => expect(router.state.location.pathname).toBe('/apps/applications'));
    await user.click(screen.getByRole('tab', { name: /App Catalog/ }));

    await waitFor(() => expect(router.state.location.pathname).toBe('/apps/catalog'));
    expect(await screen.findByText('Wikis')).toBeInTheDocument();
  });

  it('clicking Configure on a catalog card navigates to /apps/create/:appType', async () => {
    server.use(
      getListToolkitsMockHandler({ inventory: { metadata: { application: true, label: 'Inventory' } } }),
      getListApplicationsMockHandler(applicationsList(0)),
    );
    const user = userEvent.setup();
    const { router } = renderAppsRoute('/apps/catalog', { projectId: 'proj-1' });

    const configureButton = await screen.findByRole('button', { name: 'Configure' });
    await user.click(configureButton);

    await waitFor(() => expect(router.state.location.pathname).toBe('/apps/create/inventory'));
    expect(await screen.findByTestId('create-app-type-probe')).toHaveTextContent('inventory');
  });

  /** COMPOSITION ROOT — see `pages/agents/Applications.test.tsx` (issue 841). */
  it('renders both tabs inside the shared page header', async () => {
    server.use(getListApplicationsMockHandler(applicationsList(1)));
    renderAppsRoute('/apps/applications', { projectId: 'proj-1' });

    const header = await screen.findByTestId('page-header');
    expect(header).toContainElement(screen.getByRole('tablist', { name: 'Apps' }));
    expect(header).toContainElement(screen.getByRole('tab', { name: /App Catalog/ }));
  });
});
