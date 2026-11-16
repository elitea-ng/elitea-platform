import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { MarketplaceOrigin } from '@/shared/api/generated/model';

import { MarketplaceOriginBanner } from './MarketplaceOriginBanner';

const ORIGIN: MarketplaceOrigin = {
  mode: 'installed',
  listing_id: '7',
  listing_name: 'Changelog Writer',
  listing_state: 'listed',
  version_id: '311',
  version_name: 'v2.3',
  version_state: 'available',
  digest: 'abc',
  latest_version: null,
};

describe('MarketplaceOriginBanner', () => {
  it('says an installed definition is read-only and links the listing', async () => {
    const { getByTestId, getByRole, queryByTestId } = await renderBanner(ORIGIN);
    expect(getByTestId('marketplace-origin-line').textContent).toContain(
      'Installed from Changelog Writer v2.3. The definition is read-only. Fork it to change it.',
    );
    expect(getByRole('link', { name: 'Open the listing' }).getAttribute('href')).toBe('/marketplace/agents/7');
    expect(queryByTestId('marketplace-origin-withdrawn')).toBeNull();
    expect(queryByTestId('marketplace-origin-update')).toBeNull();
  });

  it('warns about a withdrawn listing, a yanked version and a newer one, in text', async () => {
    const { getByTestId } = await renderBanner({
      ...ORIGIN,
      listing_state: 'delisted',
      version_state: 'yanked',
      latest_version: { id: '400', name: 'v2.4', published_at: '2026-11-01T00:00:00Z' },
    });
    expect(getByTestId('marketplace-origin-withdrawn').textContent).toContain('Listing withdrawn');
    expect(getByTestId('marketplace-origin-yanked').textContent).toContain(
      'This version was yanked. Your agent keeps running.',
    );
    expect(getByTestId('marketplace-origin-update').textContent).toContain('Update available: v2.4.');
  });

  it('says why a version was yanked and what to do next', async () => {
    const { getByTestId } = await renderBanner({
      ...ORIGIN,
      version_state: 'yanked',
      version_state_reason: 'It leaks data.',
    });
    expect(getByTestId('marketplace-origin-yanked').textContent).toContain(
      'This version was yanked: It leaks data. Your agent keeps running. Upgrade it to another version.',
    );
  });

  it('offers Upgrade to... and the upgrade policy to an installed agent', async () => {
    const { getByTestId, queryByTestId } = await renderBanner(
      { ...ORIGIN, upgrade_policy: 'auto', latest_version: { id: '400', name: 'v2.4', published_at: '2026-11-01T00:00:00Z' }, update_widening: [{ kind: 'tool_added', detail: 'github: create_issue' }] },
      { projectId: '5', applicationId: 88 },
    );
    expect(getByTestId('marketplace-upgrade-to').textContent).toBe('Upgrade to...');
    expect(getByTestId('upgrade-auto-waiting').textContent).toContain('A new tool: github: create_issue');
    expect(queryByTestId('marketplace-review-update')).toBeNull();
  });

  it('offers Review update to a fork when upstream has a newer version, and no policy', async () => {
    const { getByTestId, queryByTestId } = await renderBanner(
      { ...ORIGIN, mode: 'forked', latest_version: { id: '400', name: 'v2.4', published_at: '2026-11-01T00:00:00Z' } },
      { projectId: '5', applicationId: 88 },
    );
    expect(getByTestId('marketplace-origin-update').textContent).toContain('Upstream update available: v2.4.');
    expect(getByTestId('marketplace-review-update').textContent).toBe('Review update');
    expect(queryByTestId('marketplace-upgrade-to')).toBeNull();
    expect(queryByTestId('upgrade-policy')).toBeNull();
  });

  it('shows a fork as a line only', async () => {
    const { getByTestId } = await renderBanner({ ...ORIGIN, mode: 'forked' });
    expect(getByTestId('marketplace-origin-line').textContent).toContain('Forked from Changelog Writer v2.3.');
    expect(getByTestId('marketplace-origin-line').textContent).not.toContain('read-only');
  });
});

/** The banner inside a real router with the listing route, so its links build real hrefs. */
async function renderBanner(origin: MarketplaceOrigin, ids?: { readonly projectId: string; readonly applicationId: number }) {
  const root = createRootRoute({ component: Outlet });
  const index = createRoute({
    getParentRoute: () => root,
    path: '/',
    component: () => (
      <QueryClientProvider client={new QueryClient()}>
        <MarketplaceOriginBanner origin={origin} projectId={ids?.projectId} applicationId={ids?.applicationId} />
      </QueryClientProvider>
    ),
  });
  const listing = createRoute({
    getParentRoute: () => root,
    path: '/marketplace/$kind/$listingId',
    component: () => null,
  });
  const router = createRouter({
    routeTree: root.addChildren([index, listing]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  });
  const view = render(<RouterProvider router={router} />);
  await waitFor(() => expect(view.getByTestId('marketplace-origin-line')).toBeTruthy());
  return view;
}
